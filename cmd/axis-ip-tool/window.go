// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"axis-ip-tool/internal/axis"
)

type glibVariant = glib.Variant

// ui runs f on the GTK main thread.
func ui(f func()) { glib.IdleAdd(func() { f() }) }

type column struct {
	title  string
	expand bool
	text   func(*axis.Device) string
	less   func(a, b *axis.Device) bool
}

var columns = []column{
	{"Model", true, func(d *axis.Device) string { return d.Model }, nil},
	{"Serial number", false, func(d *axis.Device) string { return d.Serial }, nil},
	{"Hostname", false, func(d *axis.Device) string { return d.Hostname }, nil},
	{"IPv4 address", false, func(d *axis.Device) string { return d.IP }, func(a, b *axis.Device) bool { return axis.IPLess(a.IP, b.IP) }},
	{"IPv6 address", false, func(d *axis.Device) string { return d.PreferredIPv6() }, nil},
	{"Interface", false, func(d *axis.Device) string { return d.Iface }, nil},
	{"Firmware", false, func(d *axis.Device) string { return d.Firmware }, nil},
	{"Ping", false, func(d *axis.Device) string { return d.PingText() }, nil},
	{"Connection", false, func(d *axis.Device) string { return d.Connection() }, nil},
	{"Status", false, func(d *axis.Device) string { return d.Status() }, nil},
}

// Indizes besonderer Spalten in columns
var (
	ipColumn     = 3
	ipv6Column   = 4
	pingColumn   = len(columns) - 3
	connColumn   = len(columns) - 2
	statusColumn = len(columns) - 1
)

type window struct {
	*adw.ApplicationWindow
	app *adw.Application

	reg        *axis.Registry // use in the GTK thread only
	creds      map[string][2]string
	fwd        *axis.Forwarder         // local forwardings to link-local IPv6 addresses
	infoAt     map[string]time.Time    // last query with login (refreshInfo)
	lastUser   string                  // user name entered last (preset)
	lastForAll bool                    // "for all others too" chosen last
	cells      map[string][]*gtk.Label // bound cells per device, for live updates

	keys      *gtk.StringList
	filter    *gtk.CustomFilter
	selection *gtk.MultiSelection
	stack     *gtk.Stack
	search    *gtk.SearchEntry
	toasts    *adw.ToastOverlay
	buttons   map[string]*gtk.Button

	cancelScan  context.CancelFunc
	infoPending map[string]bool         // running login checks (status column shows a spinner)
	infoRunning map[string]bool         // running refreshInfo queries (silent ones too)
	live        *gtk.Box                // "Discovery running" indicator in the header bar
	spin        map[string]*gtk.Spinner // spinner of the status cell per device
	statusParts map[uintptr]statusPart  // Statuszellen-Box → Inhalt
}

func newWindow(app *adw.Application) *window {
	w := &window{
		ApplicationWindow: adw.NewApplicationWindow(&app.Application),
		app:               app,
		reg:               axis.NewRegistry(),
		creds:             map[string][2]string{},
		cells:             map[string][]*gtk.Label{},
		buttons:           map[string]*gtk.Button{},
		infoPending:       map[string]bool{},
		infoRunning:       map[string]bool{},
		spin:              map[string]*gtk.Spinner{},
		statusParts:       map[uintptr]statusPart{},
		fwd:               axis.NewForwarder(),
		infoAt:            map[string]time.Time{},
	}
	w.SetTitle("Axis IP Tool")
	w.SetDefaultSize(1400, 640)
	glib.TimeoutAdd(1000, func() bool { w.tick(); return true })
	w.build()
	w.ConnectCloseRequest(func() bool {
		if w.cancelScan != nil {
			w.cancelScan()
		}
		return false
	})
	w.rescan()
	return w
}

// ---------------------------------------------------------------- Layout

func (w *window) build() {
	tv := adw.NewToolbarView()
	hb := adw.NewHeaderBar()

	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.SetTooltipText("Search again (F5)")
	refresh.SetActionName("win.rescan")
	hb.PackStart(refresh)

	// indicator: discovery keeps running in the background
	live := gtk.NewBox(gtk.OrientationHorizontal, 6)
	liveSpin := gtk.NewSpinner()
	liveSpin.Start()
	liveLabel := gtk.NewLabel("Discovery running")
	liveLabel.AddCSSClass("dim-label")
	live.Append(liveSpin)
	live.Append(liveLabel)
	w.live = live
	w.updateLive()
	hb.PackStart(live)

	w.search = gtk.NewSearchEntry()
	w.search.SetPlaceholderText("Filter by model, serial number, hostname, IP …")
	w.search.SetHExpand(true)
	w.search.SetMaxWidthChars(48)
	w.search.ConnectSearchChanged(func() { w.filter.Changed(gtk.FilterChangeDifferent) })
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(480)
	clamp.SetChild(w.search)
	hb.SetTitleWidget(clamp)

	menu := gio.NewMenu()
	menu.Append("Use HTTPS only", "win.https-only")
	menu.Append("Forget stored credentials", "win.forget")
	menu.Append("About Axis IP Tool", "app.about")
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetMenuModel(menu)
	mb.SetPrimary(true)
	hb.PackEnd(mb)
	tv.AddTopBar(hb)

	// table
	w.keys = gtk.NewStringList(nil)
	w.filter = gtk.NewCustomFilter(w.matches)
	filtered := gtk.NewFilterListModel(w.keys, &w.filter.Filter)
	cv := gtk.NewColumnView(nil)
	cv.SetShowColumnSeparators(true)
	cv.SetReorderable(true)
	cv.AddCSSClass("data-table")
	sorted := gtk.NewSortListModel(filtered, cv.Sorter())
	w.selection = gtk.NewMultiSelection(sorted)
	w.selection.ConnectSelectionChanged(func(_, _ uint) { w.updateActions() })
	cv.SetModel(w.selection)
	cv.ConnectActivate(func(pos uint) {
		w.selection.SelectItem(pos, true)
		w.onNetwork()
	})

	var ipCol *gtk.ColumnViewColumn
	for i, c := range columns {
		col := w.makeColumn(i, c)
		cv.AppendColumn(col)
		if i == ipColumn {
			ipCol = col
		}
	}
	cv.SortByColumn(ipCol, gtk.SortAscending)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetChild(cv)

	empty := adw.NewStatusPage()
	empty.SetIconName("camera-web-symbolic")
	empty.SetTitle("Searching for Axis devices …")
	empty.SetDescription("Devices on the local network are found via Bonjour/mDNS and SSDP.")

	w.stack = gtk.NewStack()
	w.stack.AddNamed(empty, "empty")
	w.stack.AddNamed(scroller, "list")

	// action bar
	bar := gtk.NewActionBar()
	type btn struct {
		key, label, icon string
		end              bool
		cb               func()
	}
	for _, b := range []btn{
		{"browser", "Open website", "web-browser-symbolic", false, w.onBrowser},
		{"preview", "Preview", "camera-photo-symbolic", false, w.onPreview},
		{"password", "Set password", "dialog-password-symbolic", false, w.onPassword},
		{"network", "Network", "network-wired-symbolic", false, w.onNetwork},
		{"time", "Date & time", "preferences-system-time-symbolic", false, w.onTime},
		{"restart", "Restart", "system-reboot-symbolic", true, w.onRestart},
		{"reset", "Reset", "edit-undo-symbolic", true, w.onReset},
		{"batch", "Configure multiple", "view-list-bullet-symbolic", true, w.onBatch},
	} {
		content := adw.NewButtonContent()
		content.SetLabel(b.label)
		content.SetIconName(b.icon)
		button := gtk.NewButton()
		button.SetChild(content)
		cb := b.cb
		button.ConnectClicked(func() { cb() })
		if b.end {
			bar.PackEnd(button)
		} else {
			bar.PackStart(button)
		}
		w.buttons[b.key] = button
	}
	w.buttons["restart"].AddCSSClass("destructive-action")
	w.buttons["reset"].AddCSSClass("destructive-action")
	w.buttons["reset"].SetTooltipText("Reset to factory defaults")

	tv.SetContent(w.stack)
	tv.AddBottomBar(bar)
	w.toasts = adw.NewToastOverlay()
	w.toasts.SetChild(tv)
	w.SetContent(w.toasts)

	for name, cb := range map[string]func(){
		"rescan": w.rescan,
		"search": func() { w.search.GrabFocus() },
		"forget": func() { w.creds = map[string][2]string{}; w.toast("Credentials forgotten") },
	} {
		a := gio.NewSimpleAction(name, nil)
		cb := cb
		a.ConnectActivate(func(*glib.Variant) { cb() })
		w.AddAction(a)
	}
	httpsOnly := gio.NewSimpleActionStateful("https-only", nil, glib.NewVariantBoolean(axis.HTTPSOnly()))
	httpsOnly.ConnectActivate(func(*glib.Variant) {
		on := !axis.HTTPSOnly()
		axis.SetHTTPSOnly(on)
		httpsOnly.SetState(glib.NewVariantBoolean(on))
		if err := updateSettings(func(s *settings) { s.HTTPSOnly = on }); err != nil {
			w.toast("Setting not saved: " + err.Error())
		}
		if on {
			w.toast("Encrypted connections (HTTPS) only")
		} else {
			w.toast("HTTP is allowed again if a device doesn't offer HTTPS")
		}
		w.rescan()
	})
	w.AddAction(httpsOnly)
	w.updateActions()
}

// listCell covers GtkListItem and GtkColumnViewCell.
type listCell interface {
	Item() *coreglib.Object
	Child() gtk.Widgetter
	SetChild(gtk.Widgetter)
}

func (w *window) makeColumn(idx int, c column) *gtk.ColumnViewColumn {
	f := gtk.NewSignalListItemFactory()
	f.ConnectSetup(func(obj *coreglib.Object) {
		l := gtk.NewLabel("")
		l.SetXAlign(0)
		l.SetEllipsize(pango.EllipsizeEnd)
		if idx == ipColumn {
			// double-click on the IP opens the web interface (instead of the network dialog)
			l.SetTooltipText("Double-click opens the web interface")
			click := gtk.NewGestureClick()
			click.ConnectPressed(func(n int, _, _ float64) {
				if n == 2 {
					click.SetState(gtk.EventSequenceClaimed)
					if d := w.reg.Get(w.keyOfLabel(idx, l)); d != nil && d.IP != "" {
						gtk.NewURILauncher(d.WebURL()).Launch(context.Background(), &w.Window, nil)
					}
				}
			})
			l.AddController(click)
		}
		if idx == ipv6Column {
			l.SetTooltipText("Double-click opens the web interface via IPv6")
			click := gtk.NewGestureClick()
			click.ConnectPressed(func(n int, _, _ float64) {
				if n == 2 {
					click.SetState(gtk.EventSequenceClaimed)
					if d := w.reg.Get(w.keyOfLabel(idx, l)); d != nil {
						w.openIPv6(d)
					}
				}
			})
			l.AddController(click)
		}
		if idx != statusColumn {
			obj.Cast().(listCell).SetChild(l)
			return
		}
		click := gtk.NewGestureClick()
		click.ConnectReleased(func(n int, _, _ float64) {
			if n == 1 {
				w.onStatusClicked(l)
			}
		})
		l.AddController(click)
		// status cell: spinner (during query/login) + text
		sp := gtk.NewSpinner()
		sp.SetVisible(false)
		box := gtk.NewBox(gtk.OrientationHorizontal, 6)
		box.Append(sp)
		box.Append(l)
		w.statusParts[coreglib.InternObject(box).Native()] = statusPart{l, sp}
		obj.Cast().(listCell).SetChild(box)
	})
	labelOf := func(cell listCell) (*gtk.Label, *gtk.Spinner) {
		if idx == statusColumn {
			p := w.statusParts[coreglib.InternObject(cell.Child()).Native()]
			return p.l, p.sp
		}
		return cell.Child().(*gtk.Label), nil
	}
	f.ConnectBind(func(obj *coreglib.Object) {
		cell := obj.Cast().(listCell)
		key := cell.Item().Cast().(*gtk.StringObject).String()
		l, sp := labelOf(cell)
		if sp != nil {
			w.spin[key] = sp
		}
		cells := w.cells[key]
		if len(cells) < len(columns) {
			cells = make([]*gtk.Label, len(columns))
			w.cells[key] = cells
		}
		cells[idx] = l
		if d := w.reg.Get(key); d != nil {
			w.render(key, l, idx, d)
		}
	})
	f.ConnectUnbind(func(obj *coreglib.Object) {
		cell := obj.Cast().(listCell)
		key := cell.Item().Cast().(*gtk.StringObject).String()
		l, sp := labelOf(cell)
		if cells := w.cells[key]; cells != nil && cells[idx] != nil &&
			coreglib.InternObject(cells[idx]).Native() == coreglib.InternObject(l).Native() {
			cells[idx] = nil
		}
		if sp != nil && w.spin[key] == sp {
			sp.Stop()
			delete(w.spin, key)
		}
	})
	f.ConnectTeardown(func(obj *coreglib.Object) {
		if idx == statusColumn {
			if c := obj.Cast().(listCell).Child(); c != nil {
				delete(w.statusParts, coreglib.InternObject(c).Native())
			}
		}
	})
	col := gtk.NewColumnViewColumn(c.title, &f.ListItemFactory)
	col.SetExpand(c.expand)
	col.SetResizable(true)
	less := c.less
	if less == nil {
		less = func(a, b *axis.Device) bool {
			return strings.ToLower(c.text(a)) < strings.ToLower(c.text(b))
		}
	}
	sorter := gtk.NewCustomSorter(func(a, b unsafe.Pointer) int {
		da, db := w.reg.Get(keyOf(a)), w.reg.Get(keyOf(b))
		switch {
		case da == nil || db == nil:
			return 0
		case less(da, db):
			return -1
		case less(db, da):
			return 1
		}
		return 0
	})
	col.SetSorter(&sorter.Sorter)
	return col
}

func keyOf(p unsafe.Pointer) string {
	return coreglib.Take(p).Cast().(*gtk.StringObject).String()
}

// keyOfLabel returns the device key for the (bound) cell l of column idx.
func (w *window) keyOfLabel(idx int, l *gtk.Label) string {
	native := coreglib.InternObject(l).Native()
	for key, cells := range w.cells {
		if c := cells[idx]; c != nil && coreglib.InternObject(c).Native() == native {
			return key
		}
	}
	return ""
}

type statusPart struct {
	l  *gtk.Label
	sp *gtk.Spinner
}

// busy: a query is running for the device whose result may change the status.
func (w *window) busy(key string, d *axis.Device) bool {
	return d.Online && (!d.Enriched || w.infoPending[key])
}

// rerender updates all visible cells of the device.
func (w *window) rerender(key string) {
	dev := w.reg.Get(key)
	if dev == nil {
		return
	}
	for i, l := range w.cells[key] {
		if l != nil {
			w.render(key, l, i, dev)
		}
	}
}

func (w *window) render(key string, l *gtk.Label, idx int, d *axis.Device) {
	l.SetText(columns[idx].text(d))
	if idx == pingColumn {
		l.RemoveCSSClass("warning")
		l.SetTooltipText("")
		if !d.PingAt.IsZero() {
			l.SetTooltipText("Last ping: " + d.PingAt.Format("15:04:05") + " (every second)")
			if d.Ping < 0 {
				l.AddCSSClass("warning")
			}
		}
		return
	}
	if idx == ipv6Column {
		tip := "Double-click opens the web interface via IPv6"
		if len(d.IPv6) > 1 {
			tip = "All IPv6 addresses:\n" + strings.Join(d.IPv6, "\n") + "\n\n" + tip
		}
		l.SetTooltipText(tip)
		return
	}
	if idx == connColumn {
		l.RemoveCSSClass("warning")
		l.SetTooltipText("")
		switch d.Connection() {
		case "HTTPS":
			l.SetText("🔒 HTTPS")
			l.SetTooltipText("Encrypted (certificate is not verified)")
		case "HTTP":
			l.AddCSSClass("warning")
			l.SetTooltipText("Unencrypted – device doesn't offer HTTPS")
		}
		return
	}
	if idx != statusColumn {
		return
	}
	for _, c := range []string{"success", "warning", "error", "dim-label"} {
		l.RemoveCSSClass(c)
	}
	l.SetTooltipText("")
	l.SetCursor(nil)
	busy := w.busy(key, d)
	if sp := w.spin[key]; sp != nil {
		sp.SetVisible(busy)
		if busy {
			sp.Start()
		} else {
			sp.Stop()
		}
	}
	switch {
	case busy:
		l.AddCSSClass("dim-label")
		if w.infoPending[key] {
			l.SetText("Checking login …")
		} else {
			l.SetText("Querying …")
		}
	case !d.Online:
		l.AddCSSClass("dim-label")
	case d.AuthFailed:
		l.AddCSSClass("error")
		l.SetText("🔑 " + axis.StatusAuthFailed)
		l.SetTooltipText("User name or password rejected (or no admin rights) – click to log in again")
		l.SetCursorFromName("pointer")
	case d.Status() == axis.StatusLoggedIn:
		l.AddCSSClass("success")
		l.SetText("✓ " + axis.StatusLoggedIn)
		l.SetTooltipText("Credentials verified")
	case d.Status() == axis.StatusNotLoggedIn:
		l.SetText("🔑 " + axis.StatusNotLoggedIn)
		tip := "Device is set up, password not entered yet – click to log in"
		if d.IsLocked() {
			tip += "\nThe device only shows firmware and exact model after login."
		}
		l.SetTooltipText(tip)
		l.SetCursorFromName("pointer")
	case d.NeedsSetup != nil && *d.NeedsSetup:
		l.AddCSSClass("warning")
		l.SetTooltipText("Click to create the first administrator")
		l.SetCursorFromName("pointer")
	default:
		l.AddCSSClass("warning")
	}
}

// onStatusClicked: a click on "Not logged in"/"Wrong password" logs in, on
// "No password set" sets up.
func (w *window) onStatusClicked(l *gtk.Label) {
	native := coreglib.InternObject(l).Native()
	for key, cells := range w.cells {
		if c := cells[statusColumn]; c == nil || coreglib.InternObject(c).Native() != native {
			continue
		}
		d := w.reg.Get(key)
		switch {
		case d == nil || !d.Online:
		case d.AuthFailed || d.Status() == axis.StatusNotLoggedIn:
			w.unlock(d)
		case d.NeedsSetup != nil && *d.NeedsSetup:
			w.selectKey(key)
			w.onPassword()
		}
		return
	}
}

// selectKey selects the device key in the table.
func (w *window) selectKey(key string) {
	for i := uint(0); i < w.selection.NItems(); i++ {
		if w.selection.Item(i).Cast().(*gtk.StringObject).String() == key {
			w.selection.SelectItem(i, true)
			return
		}
	}
}

// unlock asks for credentials and reads the locked device info with them.
func (w *window) unlock(d *axis.Device) {
	key := d.Key()
	w.selectKey(key)
	withLogin(w, d, "Log in", func(ctx context.Context, c *axis.Client) (axis.Device, error) {
		return loginInfo(ctx, c)
	}, func(info axis.Device) {
		if dev := w.reg.Get(key); dev != nil {
			w.onDevice(context.Background(), asUpdate(info, dev))
		}
		w.toast(label(d) + ": logged in")
	})
}

func (w *window) matches(obj *coreglib.Object) bool {
	q := strings.ToLower(strings.TrimSpace(w.search.Text()))
	if q == "" {
		return true
	}
	d := w.reg.Get(obj.Cast().(*gtk.StringObject).String())
	if d == nil {
		return false
	}
	for _, s := range []string{d.Model, d.Serial, d.Hostname, d.IP, d.PreferredIPv6(), d.Iface, d.Firmware, d.Name} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

func (w *window) toast(msg string) {
	t := adw.NewToast(msg)
	t.SetTimeout(5)
	w.toasts.AddToast(t)
}

// selected returns the selected device if exactly one is selected.
func (w *window) selected() *axis.Device {
	if devs := w.selectedDevices(); len(devs) == 1 {
		return devs[0]
	}
	return nil
}

// selectedDevices returns all selected devices in display order.
func (w *window) selectedDevices() []*axis.Device {
	set := w.selection.Selection()
	var out []*axis.Device
	for i := uint64(0); i < set.Size(); i++ {
		obj := w.selection.Item(set.Nth(uint(i)))
		if obj == nil {
			continue
		}
		if d := w.reg.Get(obj.Cast().(*gtk.StringObject).String()); d != nil {
			out = append(out, d)
		}
	}
	return out
}

func (w *window) updateActions() {
	sel := w.selectedDevices()
	usable := len(sel) == 1 && sel[0].Online && sel[0].IP != ""
	online := 0
	for _, d := range sel {
		if d.Online && d.IP != "" {
			online++
		}
	}
	for k, b := range w.buttons {
		if k == "batch" {
			b.SetSensitive(online >= 2)
			b.SetVisible(len(sel) >= 2)
			continue
		}
		b.SetSensitive(usable)
		b.SetVisible(len(sel) < 2)
	}
	n := w.keys.NItems()
	switch n {
	case 0:
		w.search.SetPlaceholderText("Filter by model, serial number, hostname, IP …")
	case 1:
		w.search.SetPlaceholderText("1 device – filter …")
	default:
		w.search.SetPlaceholderText(fmt.Sprintf("%d devices – filter by model, serial number, IP …", n))
	}
	if n > 0 {
		w.stack.SetVisibleChildName("list")
	} else {
		w.stack.SetVisibleChildName("empty")
	}
}

// ---------------------------------------------------------------- Discovery

func (w *window) rescan() {
	if w.cancelScan != nil {
		w.cancelScan()
	}
	w.keys.Splice(0, w.keys.NItems(), nil)
	w.reg = axis.NewRegistry()
	w.cells = map[string][]*gtk.Label{}
	w.spin = map[string]*gtk.Spinner{}
	w.updateActions()

	ctx, cancel := context.WithCancel(context.Background())
	w.cancelScan = cancel
	axis.Discover(ctx,
		func(d axis.Device) { ui(func() { w.onDevice(ctx, d) }) },
		func(err error) { ui(func() { w.toast(err.Error()) }) })
}

func (w *window) onDevice(ctx context.Context, d axis.Device) {
	if ctx.Err() != nil {
		return // Ergebnis eines alten Suchlaufs
	}
	var wasOnline bool
	if old := w.reg.Get(d.Key()); old != nil {
		wasOnline = old.Online
	}
	ch := w.reg.Update(d)
	key := ch.Key
	if key == "" {
		return
	}
	if dev := w.reg.Get(key); !ch.Added && ch.Removed == "" && dev != nil && dev.Online == wasOnline &&
		!d.PingAt.IsZero() {
		w.rerender(key) // pure ping result: don't refilter the list (every second per device)
		return
	}
	switch {
	case ch.Removed != "" && ch.Added:
		w.replaceKey(ch.Removed, key)
	case ch.Removed != "":
		w.replaceKey(ch.Removed, "")
	case ch.Added:
		w.keys.Append(key)
	}
	w.rerender(key)
	w.filter.Changed(gtk.FilterChangeDifferent)
	w.updateActions()
	w.refreshInfo(key)
}

// refreshInfo reads model and firmware with login if the device doesn't reveal them
// anonymously (AXIS OS 11/12 after setup) and credentials are known.
func (w *window) refreshInfo(key string) {
	d := w.reg.Get(key)
	c, ok := w.creds[key]
	// Only after the query by the discovery: cameras often report their link-local address
	// first, the real address follows fractions of a second later.
	// Afterwards repeated in the background every axis.CheckInterval (password changed? new firmware?)
	fresh := time.Since(w.infoAt[key]) < axis.CheckInterval
	if !ok || d == nil || fresh || d.IP == "" || !d.Online || !d.Enriched || w.infoPending[key] || w.infoRunning[key] {
		return
	}
	w.infoAt[key] = time.Now()
	w.infoRunning[key] = true
	// spinner only on the first check – the background repetition runs silently
	// so the status doesn't keep changing briefly.
	quiet := d.Unlocked
	if !quiet {
		w.infoPending[key] = true
		w.rerender(key)
	}
	ip, port, reg := d.IP, d.Port, w.reg
	async(func() (axis.Device, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cl := axis.NewClient(ip, port, c[0], c[1])
		return loginInfo(ctx, cl)
	}, func(info axis.Device) {
		delete(w.infoPending, key)
		delete(w.infoRunning, key)
		dev := reg.Get(key)
		if reg != w.reg || dev == nil {
			return // searched again in the meantime
		}
		w.onDevice(context.Background(), asUpdate(info, dev))
	}, func(err error) {
		delete(w.infoPending, key)
		delete(w.infoRunning, key)
		if reg != w.reg {
			return
		}
		if errors.Is(err, axis.ErrAuth) {
			w.setAuth(key, false)
			return
		}
		w.rerender(key)
		if dev := reg.Get(key); dev != nil && dev.IP != ip {
			w.refreshInfo(key) // address changed in the meantime → once more there
		}
	})
}

// setAuth records whether the credentials for key are valid (status column).
func (w *window) setAuth(key string, ok bool) {
	dev := w.reg.Get(key)
	if dev == nil {
		return
	}
	u := axis.Device{Serial: dev.Serial, IP: dev.IP, Port: dev.Port, Online: dev.Online}
	if ok {
		u.Unlocked = true
	} else {
		u.AuthFailed = true
		delete(w.creds, key)
	}
	w.onDevice(context.Background(), u)
}

// replaceKey replaces old in the list with key or removes it (key == "").
func (w *window) replaceKey(old, key string) {
	for i := uint(0); i < w.keys.NItems(); i++ {
		if w.keys.String(i) == old {
			var add []string
			if key != "" {
				add = []string{key}
			}
			w.keys.Splice(i, 1, add)
			return
		}
	}
}

// openIPv6 opens the web interface via IPv6. Link-local addresses need the interface
// (zone), which browsers don't support in URLs – hence via a local forwarding.
func (w *window) openIPv6(d *axis.Device) {
	scheme, host, port := d.WebTargetIPv6()
	if host == "" {
		return
	}
	var uri string
	if strings.Contains(host, "%") {
		local, err := w.fwd.Local(host, port)
		if err != nil {
			w.toast("Forwarding not possible: " + err.Error())
			return
		}
		uri = scheme + "://" + local + "/"
		w.toast(fmt.Sprintf("Web interface via %s (interface %s), forwarded locally", d.PreferredIPv6(), d.Iface))
	} else {
		uri = scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	}
	gtk.NewURILauncher(uri).Launch(context.Background(), &w.Window, nil)
}

// tick runs every second: devices without a sign of life for axis.MaxAge go offline,
// information requiring login is refreshed.
func (w *window) tick() {
	expired := w.reg.Expire(time.Now(), axis.MaxAge)
	for _, key := range expired {
		w.rerender(key)
	}
	if len(expired) > 0 {
		w.filter.Changed(gtk.FilterChangeDifferent)
		w.updateActions()
	}
	for key := range w.creds {
		w.refreshInfo(key)
	}
	w.updateLive()
}

// updateLive shows in the tooltip of the discovery indicator when a device answered last.
func (w *window) updateLive() {
	if w.live == nil {
		return
	}
	tip := fmt.Sprintf("Devices are monitored continuously: ping every second, "+
		"query every %d s. Without a reply for %d s a device is considered offline.",
		int(axis.CheckInterval.Seconds()), int(axis.MaxAge.Seconds()))
	var last time.Time
	for _, d := range w.reg.All() {
		if d.LastSeen.After(last) {
			last = d.LastSeen
		}
	}
	if !last.IsZero() {
		tip += "\nLast reply: " + last.Format("15:04:05")
	}
	w.live.SetTooltipText(tip)
}
