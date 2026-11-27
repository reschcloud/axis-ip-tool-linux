// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
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

	"ip-tool-for-axis/internal/axis"
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
	{"IPv4 address", false, func(d *axis.Device) string { return d.IPv4() }, func(a, b *axis.Device) bool { return axis.IPLess(a.IPv4(), b.IPv4()) }},
	{"IPv6 address", false, func(d *axis.Device) string { return d.PreferredIPv6() }, nil},
	{"Interface", false, func(d *axis.Device) string { return d.Iface }, nil},
	{"Firmware", false, firmwareText, nil},
	{"Route", false, remoteText, nil},
	{"Ping", false, func(d *axis.Device) string { return d.PingText() }, nil},
	{"Connection", false, func(d *axis.Device) string { return d.Connection() }, nil},
	{"Status", false, func(d *axis.Device) string { return d.Status() }, nil},
}

// remoteText marks devices outside the directly attached networks (traffic via router).
func remoteText(d *axis.Device) string {
	if (d.RemoteIPv4Known && d.RemoteIPv4) || (d.RemoteIPv6Known && d.RemoteIPv6) {
		return "🌐"
	}
	return ""
}

// localOrRouter describes a single address's route.
func localOrRouter(remote bool) string {
	if remote {
		return "via router"
	}
	return "local network"
}

// Indizes besonderer Spalten in columns
var (
	ipColumn     = 3
	ipv6Column   = 4
	routeColumn  = len(columns) - 4
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
	manual     []string                // manually added camera addresses (IPv4/IPv6)
	disc       *axis.Discovery         // running discovery (for manual additions)
	infoAt     map[string]time.Time    // last query with login (refreshInfo)
	lastUser   string                  // user name entered last (preset)
	lastForAll bool                    // "for all others too" chosen last
	cells      map[string][]*gtk.Label // bound cells per device, for live updates
	fwCheck    bool                    // compare firmware with axis.com (list hint)

	keys      *gtk.StringList
	filter    *gtk.CustomFilter
	selection *gtk.MultiSelection
	stack     *gtk.Stack
	search    *gtk.SearchEntry
	toasts    *adw.ToastOverlay
	buttons   map[string]*gtk.Button
	contents  map[string]*adw.ButtonContent
	locks     map[string]*gtk.Image

	cancelScan  context.CancelFunc
	infoPending map[string]bool         // running login checks (status column shows a spinner)
	infoRunning map[string]bool         // running refreshInfo queries (silent ones too)
	loginOpen   map[string]bool         // a login dialog is already open for this device
	live        *gtk.Box                // "Discovery running" indicator in the header bar
	spin        map[string]*gtk.Spinner // spinner of the status cell per device
	statusParts map[uintptr]statusPart  // Statuszellen-Box → Inhalt
}

func newWindow(app *adw.Application) *window {
	installStyles()
	s, _ := loadSettings()
	w := &window{
		ApplicationWindow: adw.NewApplicationWindow(&app.Application),
		app:               app,
		reg:               axis.NewRegistry(),
		creds:             map[string][2]string{},
		manual:            s.Manual,
		cells:             map[string][]*gtk.Label{},
		buttons:           map[string]*gtk.Button{},
		contents:          map[string]*adw.ButtonContent{},
		locks:             map[string]*gtk.Image{},
		infoPending:       map[string]bool{},
		infoRunning:       map[string]bool{},
		loginOpen:         map[string]bool{},
		spin:              map[string]*gtk.Spinner{},
		statusParts:       map[uintptr]statusPart{},
		fwd:               axis.NewForwarder(),
		fwCheck:           firmwareCheckEnabled(),
		infoAt:            map[string]time.Time{},
	}
	w.SetTitle(appName)
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

	add := gtk.NewButtonFromIconName("list-add-symbolic")
	add.SetTooltipText("Add cameras manually")
	add.ConnectClicked(func() { w.onAddManual() })
	hb.PackStart(add)

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
	w.search.ConnectSearchChanged(func() {
		w.filter.Changed(gtk.FilterChangeDifferent)
		w.updateActions()
	})
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(480)
	clamp.SetChild(w.search)
	hb.SetTitleWidget(clamp)

	menu := gio.NewMenu()
	menu.Append("Use HTTPS only", "win.https-only")
	menu.Append("Check axis.com for firmware updates", "win.fw-check")
	menu.Append("Manual cameras", "win.manual")
	menu.Append("Forget stored credentials", "win.forget")
	menu.Append("About "+appName, "app.about")
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetMenuModel(menu)
	mb.SetPrimary(true)
	hb.PackEnd(mb)
	hb.PackEnd(w.appUpdateButton())
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

	noMatches := adw.NewStatusPage()
	noMatches.SetIconName("edit-find-symbolic")
	noMatches.SetTitle("No devices match the filter")
	noMatches.SetDescription("Change the search text or clear the filter to see all devices.")
	clearFilter := gtk.NewButtonWithLabel("Clear filter")
	clearFilter.AddCSSClass("suggested-action")
	clearFilter.ConnectClicked(func() { w.search.SetText("") })
	noMatches.SetChild(clearFilter)

	w.stack = gtk.NewStack()
	w.stack.AddNamed(empty, "empty")
	w.stack.AddNamed(noMatches, "nofilter")
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
		{"preview", "Live view", "camera-video-symbolic", false, w.onPreview},
		{"password", "Set password", "dialog-password-symbolic", false, w.onPassword},
		{"network", "Network", "network-wired-symbolic", false, w.onNetwork},
		{"time", "Date & time", "preferences-system-time-symbolic", false, w.onTime},
		{"restart", "Restart", "system-reboot-symbolic", true, w.onRestart},
		{"reset", "Reset", "edit-undo-symbolic", true, w.onReset},
		{"firmware", "Firmware", "software-update-available-symbolic", true, w.onFirmware},
		{"batch", "Configure multiple", "view-list-bullet-symbolic", true, w.onBatch},
	} {
		content := adw.NewButtonContent()
		content.SetLabel(b.label)
		content.SetIconName(b.icon)
		lock := gtk.NewImageFromIconName("system-lock-screen-symbolic")
		lock.SetPixelSize(14)
		lock.SetVAlign(gtk.AlignCenter)
		lock.SetVisible(false)
		box := gtk.NewBox(gtk.OrientationHorizontal, 6)
		box.Append(content)
		box.Append(lock)
		button := gtk.NewButton()
		button.SetChild(box)
		button.AddCSSClass("action-button")
		key := b.key
		cb := b.cb
		button.ConnectClicked(func() {
			// Without a login, most actions first open the login dialog. The lock
			// icon shows that; the buttons stay clickable. The website and live view
			// work without a login too (anonymous viewers, web UI login).
			if !actionWithoutLogin(key) {
				if d := w.selected(); d != nil && requiresLogin(d) {
					w.unlockThen(d, cb)
					return
				}
			}
			cb()
		})
		if b.end {
			bar.PackEnd(button)
		} else {
			bar.PackStart(button)
		}
		w.buttons[key] = button
		w.contents[key] = content
		w.locks[key] = lock
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
		"manual": w.onAddManual,
		"forget": w.forgetCredentials,
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
	fwCheck := gio.NewSimpleActionStateful("fw-check", nil, glib.NewVariantBoolean(w.fwCheck))
	fwCheck.ConnectActivate(func(*glib.Variant) {
		w.fwCheck = !w.fwCheck
		fwCheck.SetState(glib.NewVariantBoolean(w.fwCheck))
		off := !w.fwCheck
		if err := updateSettings(func(s *settings) { s.NoFirmwareCheck = off }); err != nil {
			w.toast("Setting not saved: " + err.Error())
		}
		if off {
			clear(fwHints)
		}
		for _, d := range w.reg.All() {
			w.checkFirmware(d.Key())
			w.rerender(d.Key())
		}
	})
	w.AddAction(fwCheck)
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
						w.openWeb(d)
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
			tip := "Last ping: " + d.PingAt.Format("15:04:05") + " (every 2 s)"
			switch {
			case d.Ping < 0 && d.PingAddr != "":
				tip += "\nNo reply from " + d.PingAddr
			case d.Ping < 0:
				tip += "\nNo reply"
			case d.PingAddr != "":
				tip += "\nvia " + d.PingAddr
				if f := axis.AddrFamily(d.PingAddr); f != "" {
					tip += " (" + f + ")"
				}
			}
			l.SetTooltipText(tip)
			if d.Ping < 0 {
				l.AddCSSClass("warning")
			}
		}
		return
	}
	if idx == ipv6Column {
		if d.PreferredIPv6() == "" {
			l.SetTooltipText("No IPv6 address known")
			return
		}
		tip := "Double-click opens the web interface via IPv6"
		if len(d.IPv6) > 1 {
			tip = "All IPv6 addresses:\n" + strings.Join(d.IPv6, "\n") + "\n\n" + tip
		}
		l.SetTooltipText(tip)
		return
	}
	if idx == routeColumn {
		l.RemoveCSSClass("dim-label")
		l.SetTooltipText("")
		var parts []string
		if d.RemoteIPv4Known {
			parts = append(parts, "IPv4: "+localOrRouter(d.RemoteIPv4))
		}
		if d.RemoteIPv6Known {
			parts = append(parts, "IPv6: "+localOrRouter(d.RemoteIPv6))
		}
		if len(parts) > 0 {
			l.SetTooltipText("Route – " + strings.Join(parts, ", "))
		}
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
			if d.Scheme == "http" {
				l.SetTooltipText("Unencrypted – explicitly selected in the manual URL")
			} else {
				l.SetTooltipText("Unencrypted – device doesn't offer HTTPS")
			}
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
	case d.Status() == axis.StatusUnresolved:
		l.AddCSSClass("warning")
		l.SetText("⚠ " + axis.StatusUnresolved)
		l.SetTooltipText("The hostname could not be resolved. Check DNS or enter an IP address.")
	case d.Status() == axis.StatusNoVAPIX:
		l.AddCSSClass("warning")
		l.SetText("⚠ " + axis.StatusNoVAPIX)
		l.SetTooltipText("The device answers, but not with the Axis VAPIX API.")
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
			w.passwordAction(d)
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
func (w *window) unlock(d *axis.Device) { w.unlockThen(d, nil) }

// unlockThen logs in and, after success, runs after (e.g. the action the user
// originally clicked). This avoids having to click the same button twice.
func (w *window) unlockThen(d *axis.Device, after func()) {
	key := d.Key()
	w.selectKey(key)
	withLogin(w, d, "Log in", func(ctx context.Context, c *axis.Client) (axis.Device, error) {
		return loginInfo(ctx, c)
	}, func(info axis.Device) {
		if dev := w.reg.Get(key); dev != nil {
			w.onDevice(context.Background(), asUpdate(info, dev))
		}
		w.toast(label(d) + ": logged in")
		if after != nil {
			after()
		}
	})
}

func (w *window) matches(obj *coreglib.Object) bool {
	d := w.reg.Get(obj.Cast().(*gtk.StringObject).String())
	return deviceMatches(d, w.search.Text())
}

// deviceMatches is the filter predicate (also used to count matches).
func deviceMatches(d *axis.Device, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	if d == nil {
		return false
	}
	route := ""
	if d.RemoteIPv4Known || d.RemoteIPv6Known {
		route = "local"
		if remoteText(d) != "" {
			route = "remote"
		}
	}
	for _, s := range []string{d.Model, d.Serial, d.Hostname, d.IP, d.IPv4(), d.PreferredIPv6(),
		d.Iface, d.Firmware, d.Name, d.Status(), d.Connection(), route} {
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

// forgetCredentials clears the stored logins and resets the visible login state.
func (w *window) forgetCredentials() {
	w.creds = map[string][2]string{}
	w.infoAt = map[string]time.Time{}
	w.reg.ClearAuth()
	for _, d := range w.reg.All() {
		w.rerender(d.Key())
	}
	w.updateActions()
	w.toast("Credentials forgotten – log in again when needed")
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
	locked := len(sel) == 1 && requiresLogin(sel[0])
	for k, b := range w.buttons {
		if img := w.locks[k]; img != nil {
			img.SetVisible(locked && !actionWithoutLogin(k))
		}
		if k == "batch" {
			b.SetSensitive(online >= 2)
			b.SetVisible(len(sel) >= 2)
			continue
		}
		if k == "firmware" {
			b.SetSensitive(online >= 1)
			b.SetTooltipText("Compare with axis.com and update the firmware")
		} else if k == "password" {
			b.SetSensitive(usable)
			b.SetVisible(len(sel) < 2)
			switch {
			case len(sel) == 1 && sel[0].Unlocked:
				w.contents[k].SetLabel("Change password")
				b.SetTooltipText("Change the password of a user")
			case len(sel) == 1 && !needsInitialUser(sel[0]):
				w.contents[k].SetLabel("Change password")
				b.SetTooltipText("Log in first – then the password can be changed")
			default:
				w.contents[k].SetLabel("Set password")
				b.SetTooltipText("Create the first administrator (factory state)")
			}
		} else {
			b.SetSensitive(usable)
			b.SetVisible(len(sel) < 2)
			b.SetTooltipText("")
			switch k {
			case "reset":
				b.SetTooltipText("Reset to factory defaults")
			case "preview":
				b.SetTooltipText("Tries anonymous access; asks for login only if the camera requires it")
			case "browser":
				b.SetTooltipText("Opens the web interface – log in there if required")
			}
		}
		if locked && !actionWithoutLogin(k) {
			if k == "password" {
				b.SetTooltipText("Log in required – then the password can be changed")
			} else {
				b.SetTooltipText("Log in required – click to log in")
			}
		}
	}
	n := w.keys.NItems()
	shown := uint(0)
	for _, d := range w.reg.All() {
		if deviceMatches(d, w.search.Text()) {
			shown++
		}
	}
	switch n {
	case 0:
		w.search.SetPlaceholderText("Filter by model, serial number, hostname, IP …")
	case 1:
		w.search.SetPlaceholderText("1 device – filter …")
	default:
		if shown > 0 && shown < n {
			w.search.SetPlaceholderText(fmt.Sprintf("%d of %d devices – filter …", shown, n))
		} else {
			w.search.SetPlaceholderText(fmt.Sprintf("%d devices – filter by model, serial number, hostname, IP …", n))
		}
	}
	switch {
	case n == 0:
		w.stack.SetVisibleChildName("empty")
	case shown == 0:
		w.stack.SetVisibleChildName("nofilter")
	default:
		w.stack.SetVisibleChildName("list")
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
	w.disc = axis.Discover(ctx,
		func(d axis.Device) { ui(func() { w.onDevice(ctx, d) }) },
		func(err error) { ui(func() { w.toast(err.Error()) }) })
	for _, addr := range w.manual {
		if err := w.disc.AddManual(addr); err != nil {
			w.toast("Manual camera " + addr + ": " + err.Error())
		}
	}
}

// onAddManual opens the dialog for manually added cameras: enter one or more targets
// and see the entries that are currently monitored.
func (w *window) onAddManual() {
	dlg := newFormDialog("Manual cameras", "Add", 520)
	entry := entryRow("Addresses, hostnames, URLs or ranges", "")
	dlg.add(note("Adds cameras that discovery doesn't find – for example in another " +
		"subnet, via hostname or IPv6. Entries may be IP addresses, hostnames, complete " +
		"http(s) URLs, CIDR prefixes (192.168.0.0/24), ranges (192.168.0.10-192.168.0.20, " +
		"192.168.0.10-20) or wildcards (192.168.0.*). Separate several entries with comma, " +
		"semicolon and/or spaces. Ping, offline detection and the VAPIX queries run exactly " +
		"like for discovered devices. For a link-local IPv6 address append the interface, " +
		"e.g. fe80::1234%eth0."))
	dlg.add(boxedList(entry))

	lb := gtk.NewListBox()
	lb.SetSelectionMode(gtk.SelectionNone)
	lb.AddCSSClass("boxed-list")
	if len(w.manual) > 0 {
		dlg.add(heading("Monitored entries"))
		for _, spec := range w.manual {
			lb.Append(w.manualRow(lb, spec))
		}
		dlg.add(lb)
	}
	dlg.apply.ConnectClicked(func() {
		raw := entry.Text()
		dlg.busy(true)
		// Parsing/expanding large ranges can take a moment; keep the GTK thread free.
		async(func() ([]axis.ManualEntry, error) {
			return axis.ParseManual(raw)
		}, func(entries []axis.ManualEntry) {
			dlg.busy(false)
			n, err := w.addParsed(entries)
			if err != nil {
				dlg.toast(err.Error())
				return
			}
			dlg.Close()
			if n == 1 {
				w.toast("Monitoring 1 camera")
			} else {
				w.toast(fmt.Sprintf("Monitoring %d cameras", n))
			}
		}, func(err error) {
			dlg.busy(false)
			dlg.toast(err.Error())
		})
	})
	dlg.Present(w)
	entry.GrabFocus()
}

// manualRow shows a manually added entry with a button to stop monitoring it.
func (w *window) manualRow(lb *gtk.ListBox, spec string) gtk.Widgetter {
	r := adw.NewActionRow()
	r.SetTitle(spec)
	r.SetSubtitle("Added manually")
	rm := gtk.NewButtonFromIconName("list-remove-symbolic")
	rm.SetTooltipText("Stop monitoring this entry")
	rm.SetVAlign(gtk.AlignCenter)
	rm.AddCSSClass("flat")
	rm.ConnectClicked(func() {
		w.removeManual(spec)
		lb.Remove(r)
	})
	r.AddSuffix(rm)
	return r
}

// addParsed starts monitoring all new entries and stores them in the settings. It
// returns the number of cameras (ranges may contain several).
func (w *window) addParsed(entries []axis.ManualEntry) (int, error) {
	var added []axis.ManualEntry
	cameras := 0
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Spec] || slices.Contains(w.manual, e.Spec) {
			continue
		}
		seen[e.Spec] = true
		added = append(added, e)
		cameras += len(e.Devices)
	}
	if len(added) == 0 {
		return 0, errors.New("these cameras are already being monitored")
	}
	for _, e := range added {
		w.manual = append(w.manual, e.Spec)
		if w.disc != nil {
			for _, dev := range e.Devices {
				w.disc.AddDevice(dev)
			}
		}
	}
	if err := updateSettings(func(s *settings) { s.Manual = w.manual }); err != nil {
		w.toast("Entries not saved: " + err.Error())
	}
	return cameras, nil
}

// removeManual stops monitoring an entry and updates the stored list. The device list
// is rebuilt by a new discovery run (remaining entries are added to it again).
func (w *window) removeManual(spec string) {
	i := slices.Index(w.manual, spec)
	if i < 0 {
		return
	}
	w.manual = slices.Delete(w.manual, i, i+1)
	if err := updateSettings(func(s *settings) { s.Manual = w.manual }); err != nil {
		w.toast("Setting not saved: " + err.Error())
	}
	w.forgetRemoved(spec)
	w.rescan()
	w.toast("No longer monitoring " + spec)
}

// forgetRemoved drops credentials/timestamps of devices that belonged to spec, so a
// camera later reusing the address doesn't get the old password.
func (w *window) forgetRemoved(spec string) {
	entries, err := axis.ParseManual(spec)
	if err != nil {
		return
	}
	match := map[string]bool{}
	for _, e := range entries {
		for _, dev := range e.Devices {
			if dev.IP != "" {
				match[dev.IP] = true
				match[strings.ToLower(dev.IP)] = true
			}
			if dev.Hostname != "" {
				match[strings.ToLower(dev.Hostname)] = true
			}
		}
	}
	for _, d := range w.reg.All() {
		if !match[d.IP] && !match[strings.ToLower(d.IP)] && !match[strings.ToLower(d.Hostname)] &&
			!match[d.ResolvedIPv4] {
			continue
		}
		delete(w.creds, d.Key())
		delete(w.infoAt, d.Key())
		delete(w.infoPending, d.Key())
		delete(w.infoRunning, d.Key())
	}
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
		w.rerender(key) // pure ping result: don't refilter the list (every 2 s per device)
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
	if ch.Removed != "" {
		w.moveKeyState(ch.Removed, key)
	}
	w.rerender(key)
	w.filter.Changed(gtk.FilterChangeDifferent)
	w.updateActions()
	w.refreshInfo(key)
	w.checkFirmware(key)
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
	snap, reg := *d, w.reg
	ip := snap.IP
	async(func() (axis.Device, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return loginInfo(ctx, snap.Client(c[0], c[1]))
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
		if errors.Is(err, axis.ErrNoUser) {
			delete(w.creds, key)
			if dev := reg.Get(key); dev != nil {
				yes := true
				w.onDevice(context.Background(), axis.Device{Serial: dev.Serial, IP: dev.IP,
					Port: dev.Port, Online: dev.Online, NeedsSetup: &yes})
			}
			return
		}
		if errors.Is(err, axis.ErrAuth) {
			w.setAuth(key, false)
			return
		}
		w.rerender(key)
		if dev := reg.Get(key); dev != nil && dev.IP != ip {
			delete(w.infoAt, key) // force a new attempt at the changed address
			w.refreshInfo(key)
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

// moveKeyState moves credentials and query timestamps when a device is re-keyed
// (e.g. IP → serial number).
func (w *window) moveKeyState(old, key string) {
	if old == "" || old == key {
		return
	}
	if key != "" {
		if c, ok := w.creds[old]; ok {
			w.creds[key] = c
		}
		if t, ok := w.infoAt[old]; ok {
			w.infoAt[key] = t
		}
		if w.infoPending[old] {
			w.infoPending[key] = true
		}
		if w.infoRunning[old] {
			w.infoRunning[key] = true
		}
	}
	delete(w.creds, old)
	delete(w.infoAt, old)
	delete(w.infoPending, old)
	delete(w.infoRunning, old)
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

// openWeb opens the web interface of d. Link-local IPv6 addresses need a local
// forwarding because browsers don't support zones in URLs.
func (w *window) openWeb(d *axis.Device) {
	if strings.Contains(d.IP, "%") {
		w.openIPv6(d)
		return
	}
	gtk.NewURILauncher(d.WebURL()).Launch(context.Background(), &w.Window, nil)
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
	tip := fmt.Sprintf("Devices are monitored continuously: ping every %d s, "+
		"query every %d s. Offline after %d s without a reply (%d s if the device never "+
		"answers pings).", int(axis.PingInterval.Seconds()), int(axis.CheckInterval.Seconds()),
		int(axis.MaxAge.Seconds()), int(axis.MaxAgeNoPing.Seconds()))
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
