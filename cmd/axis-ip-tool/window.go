// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
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
	{"Name", true, func(d *axis.Device) string { return d.Name }, nil},
	{"Hostname", false, func(d *axis.Device) string { return d.Hostname }, nil},
	{"IP-Adresse", false, func(d *axis.Device) string { return d.IP }, func(a, b *axis.Device) bool { return axis.IPLess(a.IP, b.IP) }},
	{"Seriennummer", false, func(d *axis.Device) string { return d.Serial }, nil},
	{"Modell", true, func(d *axis.Device) string { return d.Model }, nil},
	{"Firmware", false, func(d *axis.Device) string { return d.Firmware }, nil},
	{"Verbindung", false, func(d *axis.Device) string { return d.Connection() }, nil},
	{"Status", false, func(d *axis.Device) string { return d.Status() }, nil},
}

// Indizes besonderer Spalten in columns
var (
	ipColumn     = 2
	connColumn   = len(columns) - 2
	statusColumn = len(columns) - 1
)

type window struct {
	*adw.ApplicationWindow
	app *adw.Application

	reg        *axis.Registry // nur im GTK-Thread verwenden
	creds      map[string][2]string
	lastUser   string                  // zuletzt eingegebener Benutzername (Vorbelegung)
	lastForAll bool                    // „auch bei allen anderen“ zuletzt gewählt
	cells      map[string][]*gtk.Label // gebundene Zellen je Gerät, für Live-Updates

	keys      *gtk.StringList
	filter    *gtk.CustomFilter
	selection *gtk.MultiSelection
	stack     *gtk.Stack
	search    *gtk.SearchEntry
	toasts    *adw.ToastOverlay
	buttons   map[string]*gtk.Button

	cancelScan  context.CancelFunc
	infoPending map[string]bool         // laufende Anmeldeprüfungen (Statusspalte zeigt Spinner)
	spin        map[string]*gtk.Spinner // Spinner der Statuszelle je Gerät
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
		spin:              map[string]*gtk.Spinner{},
		statusParts:       map[uintptr]statusPart{},
	}
	w.SetTitle("Axis IP Tool")
	w.SetDefaultSize(1100, 640)
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

// ---------------------------------------------------------------- Aufbau

func (w *window) build() {
	tv := adw.NewToolbarView()
	hb := adw.NewHeaderBar()

	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.SetTooltipText("Neu suchen (F5)")
	refresh.SetActionName("win.rescan")
	hb.PackStart(refresh)

	w.search = gtk.NewSearchEntry()
	w.search.SetPlaceholderText("Filtern nach Name, IP, Seriennummer …")
	w.search.SetHExpand(true)
	w.search.SetMaxWidthChars(48)
	w.search.ConnectSearchChanged(func() { w.filter.Changed(gtk.FilterChangeDifferent) })
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(480)
	clamp.SetChild(w.search)
	hb.SetTitleWidget(clamp)

	menu := gio.NewMenu()
	menu.Append("Nur HTTPS verwenden", "win.https-only")
	menu.Append("Gespeicherte Anmeldedaten vergessen", "win.forget")
	menu.Append("Über Axis IP Tool", "app.about")
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetMenuModel(menu)
	mb.SetPrimary(true)
	hb.PackEnd(mb)
	tv.AddTopBar(hb)

	// Tabelle
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
		if i == 1 {
			ipCol = col
		}
	}
	cv.SortByColumn(ipCol, gtk.SortAscending)

	scroller := gtk.NewScrolledWindow()
	scroller.SetVExpand(true)
	scroller.SetChild(cv)

	empty := adw.NewStatusPage()
	empty.SetIconName("camera-web-symbolic")
	empty.SetTitle("Suche nach Axis-Geräten …")
	empty.SetDescription("Geräte im lokalen Netz werden per Bonjour/mDNS und SSDP gefunden.")

	w.stack = gtk.NewStack()
	w.stack.AddNamed(empty, "empty")
	w.stack.AddNamed(scroller, "list")

	// Aktionsleiste
	bar := gtk.NewActionBar()
	type btn struct {
		key, label, icon string
		end              bool
		cb               func()
	}
	for _, b := range []btn{
		{"browser", "Webseite öffnen", "web-browser-symbolic", false, w.onBrowser},
		{"preview", "Vorschau", "camera-photo-symbolic", false, w.onPreview},
		{"password", "Passwort setzen", "dialog-password-symbolic", false, w.onPassword},
		{"network", "Netzwerk", "network-wired-symbolic", false, w.onNetwork},
		{"time", "Datum & Zeit", "preferences-system-time-symbolic", false, w.onTime},
		{"restart", "Neustart", "system-reboot-symbolic", true, w.onRestart},
		{"reset", "Zurücksetzen", "edit-undo-symbolic", true, w.onReset},
		{"batch", "Mehrere konfigurieren", "view-list-bullet-symbolic", true, w.onBatch},
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
	w.buttons["reset"].SetTooltipText("Auf Werkseinstellungen zurücksetzen")

	tv.SetContent(w.stack)
	tv.AddBottomBar(bar)
	w.toasts = adw.NewToastOverlay()
	w.toasts.SetChild(tv)
	w.SetContent(w.toasts)

	for name, cb := range map[string]func(){
		"rescan": w.rescan,
		"search": func() { w.search.GrabFocus() },
		"forget": func() { w.creds = map[string][2]string{}; w.toast("Anmeldedaten vergessen") },
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
		if err := saveSettings(settings{HTTPSOnly: on}); err != nil {
			w.toast("Einstellung nicht gespeichert: " + err.Error())
		}
		if on {
			w.toast("Nur noch verschlüsselte Verbindungen (HTTPS)")
		} else {
			w.toast("HTTP ist wieder erlaubt, wenn ein Gerät kein HTTPS anbietet")
		}
		w.rescan()
	})
	w.AddAction(httpsOnly)
	w.updateActions()
}

// listCell deckt GtkListItem und GtkColumnViewCell ab.
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
			// Doppelklick auf die IP öffnet die Weboberfläche (statt des Netzwerk-Dialogs)
			l.SetTooltipText("Doppelklick öffnet die Weboberfläche")
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
		// Statuszelle: Spinner (während Abfrage/Anmeldung) + Text
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

// keyOfLabel liefert den Geräteschlüssel zur (gebundenen) Zelle l der Spalte idx.
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

// busy: für das Gerät läuft gerade eine Abfrage, deren Ergebnis den Status ändern kann.
func (w *window) busy(key string, d *axis.Device) bool {
	return d.Online && (!d.Enriched || w.infoPending[key])
}

// rerender aktualisiert alle sichtbaren Zellen des Geräts.
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
	if idx == connColumn {
		l.RemoveCSSClass("warning")
		l.SetTooltipText("")
		switch d.Connection() {
		case "HTTPS":
			l.SetText("🔒 HTTPS")
			l.SetTooltipText("Verschlüsselt (Zertifikat wird nicht geprüft)")
		case "HTTP":
			l.AddCSSClass("warning")
			l.SetTooltipText("Unverschlüsselt – Gerät bietet kein HTTPS an")
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
			l.SetText("Anmeldung wird geprüft …")
		} else {
			l.SetText("Wird abgefragt …")
		}
	case !d.Online:
		l.AddCSSClass("dim-label")
	case d.AuthFailed:
		l.AddCSSClass("error")
		l.SetText("🔑 Passwort falsch")
		l.SetTooltipText("Benutzername oder Passwort abgelehnt (oder keine Admin-Rechte) – klicken zum erneuten Anmelden")
		l.SetCursorFromName("pointer")
	case d.Status() == "Angemeldet":
		l.AddCSSClass("success")
		l.SetText("✓ Angemeldet")
		l.SetTooltipText("Zugangsdaten geprüft")
	case d.Status() == "Nicht angemeldet":
		l.SetText("🔑 Nicht angemeldet")
		tip := "Gerät ist eingerichtet, Passwort noch nicht eingegeben – klicken zum Anmelden"
		if d.IsLocked() {
			tip += "\nDas Gerät zeigt Firmware und genaues Modell erst nach der Anmeldung."
		}
		l.SetTooltipText(tip)
		l.SetCursorFromName("pointer")
	case d.NeedsSetup != nil && *d.NeedsSetup:
		l.AddCSSClass("warning")
		l.SetTooltipText("Klicken, um den ersten Administrator anzulegen")
		l.SetCursorFromName("pointer")
	default:
		l.AddCSSClass("warning")
	}
}

// onStatusClicked: Klick auf „Nicht angemeldet“/„Passwort falsch“ meldet an, auf
// „Kein Passwort gesetzt“ richtet ein.
func (w *window) onStatusClicked(l *gtk.Label) {
	native := coreglib.InternObject(l).Native()
	for key, cells := range w.cells {
		if c := cells[statusColumn]; c == nil || coreglib.InternObject(c).Native() != native {
			continue
		}
		d := w.reg.Get(key)
		switch {
		case d == nil || !d.Online:
		case d.AuthFailed || d.Status() == "Nicht angemeldet":
			w.unlock(d)
		case d.NeedsSetup != nil && *d.NeedsSetup:
			w.selectKey(key)
			w.onPassword()
		}
		return
	}
}

// selectKey wählt das Gerät key in der Tabelle aus.
func (w *window) selectKey(key string) {
	for i := uint(0); i < w.selection.NItems(); i++ {
		if w.selection.Item(i).Cast().(*gtk.StringObject).String() == key {
			w.selection.SelectItem(i, true)
			return
		}
	}
}

// unlock fragt Zugangsdaten ab und liest damit die gesperrten Geräteinfos.
func (w *window) unlock(d *axis.Device) {
	key := d.Key()
	w.selectKey(key)
	withLogin(w, d, "Anmelden", func(ctx context.Context, c *axis.Client) (axis.Device, error) {
		return loginInfo(ctx, c)
	}, func(info axis.Device) {
		if dev := w.reg.Get(key); dev != nil {
			w.onDevice(context.Background(), asUpdate(info, dev))
		}
		w.toast(label(d) + ": angemeldet")
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
	for _, s := range []string{d.Name, d.Hostname, d.IP, d.Serial, d.Model, d.Firmware} {
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

// selected liefert das ausgewählte Gerät, wenn genau eines ausgewählt ist.
func (w *window) selected() *axis.Device {
	if devs := w.selectedDevices(); len(devs) == 1 {
		return devs[0]
	}
	return nil
}

// selectedDevices liefert alle ausgewählten Geräte in Anzeigereihenfolge.
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
		w.search.SetPlaceholderText("Filtern nach Name, IP, Seriennummer …")
	case 1:
		w.search.SetPlaceholderText("1 Gerät – filtern …")
	default:
		w.search.SetPlaceholderText(fmt.Sprintf("%d Geräte – filtern nach Name, IP, Seriennummer …", n))
	}
	if n > 0 {
		w.stack.SetVisibleChildName("list")
	} else {
		w.stack.SetVisibleChildName("empty")
	}
}

// ---------------------------------------------------------------- Suche

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
	ch := w.reg.Update(d)
	key := ch.Key
	if key == "" {
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

// refreshInfo liest Modell und Firmware mit Anmeldung nach, wenn das Gerät sie
// anonym nicht herausgibt (AXIS OS 11/12 nach Einrichtung) und Zugangsdaten bekannt sind.
func (w *window) refreshInfo(key string) {
	d := w.reg.Get(key)
	c, ok := w.creds[key]
	// Erst nach der Abfrage durch die Suche: Die Kameras melden sich oft zuerst mit der
	// Link-Local-Adresse, die richtige Adresse folgt Sekundenbruchteile später.
	if !ok || d == nil || d.Unlocked || d.IP == "" || !d.Online || !d.Enriched || w.infoPending[key] {
		return
	}
	w.infoPending[key] = true
	w.rerender(key)
	ip, port, reg := d.IP, d.Port, w.reg
	async(func() (axis.Device, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cl := axis.NewClient(ip, port, c[0], c[1])
		return loginInfo(ctx, cl)
	}, func(info axis.Device) {
		delete(w.infoPending, key)
		dev := reg.Get(key)
		if reg != w.reg || dev == nil {
			return // inzwischen neu gesucht
		}
		w.onDevice(context.Background(), asUpdate(info, dev))
	}, func(err error) {
		delete(w.infoPending, key)
		if reg != w.reg {
			return
		}
		if errors.Is(err, axis.ErrAuth) {
			w.setAuth(key, false)
			return
		}
		w.rerender(key)
		if dev := reg.Get(key); dev != nil && dev.IP != ip {
			w.refreshInfo(key) // Adresse hat sich inzwischen geändert → dort noch einmal
		}
	})
}

// setAuth vermerkt, ob die Zugangsdaten für key gültig sind (Statusspalte).
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

// replaceKey ersetzt old in der Liste durch key oder entfernt es (key == "").
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
