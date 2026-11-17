// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"axis-ip-tool/internal/axis"
)

// Mehrfachkonfiguration: Einstellungen → Vorschau (editierbar) → Ausführung.

type batchDialog struct {
	w    *window
	dlg  *adw.Dialog
	nav  *adw.NavigationView
	devs []axis.Device
	all  []axis.Device

	settings axis.BatchSettings
	plan     []axis.PlanRow
	planName string
	as       []axis.Assignment
	inUse    map[string]bool // IP → antwortet bereits

	toasts *adw.ToastOverlay
}

func (w *window) onBatch() {
	sel := w.selectedDevices()
	b := &batchDialog{w: w, inUse: map[string]bool{}}
	for _, d := range sel {
		if d.Online && d.IP != "" {
			b.devs = append(b.devs, *d)
		}
	}
	if len(b.devs) < 2 {
		w.toast("Bitte mindestens zwei erreichbare Geräte auswählen")
		return
	}
	for _, d := range w.reg.All() {
		b.all = append(b.all, *d)
	}
	b.dlg = adw.NewDialog()
	b.dlg.SetTitle("Mehrere Geräte konfigurieren")
	b.dlg.SetContentWidth(720)
	b.dlg.SetContentHeight(680)
	b.nav = adw.NewNavigationView()
	b.toasts = adw.NewToastOverlay()
	b.toasts.SetChild(b.nav)
	b.dlg.SetChild(b.toasts)
	b.nav.Add(b.settingsPage())
	b.dlg.Present(w)
}

func (b *batchDialog) toast(msg string) {
	t := adw.NewToast(msg)
	t.SetTimeout(6)
	b.toasts.AddToast(t)
}

// page baut eine Navigationsseite mit Kopfzeile und scrollbarem Inhalt.
func page(title string, start, end gtk.Widgetter, content gtk.Widgetter) *adw.NavigationPage {
	hb := adw.NewHeaderBar()
	hb.SetShowEndTitleButtons(false)
	if start != nil {
		hb.PackStart(start)
	}
	if end != nil {
		hb.PackEnd(end)
	}
	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetVExpand(true)
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(680)
	clamp.SetChild(content)
	scroller.SetChild(clamp)
	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(scroller)
	return adw.NewNavigationPage(tv, title)
}

func vbox() *gtk.Box {
	box := gtk.NewBox(gtk.OrientationVertical, 18)
	for _, f := range []func(int){box.SetMarginTop, box.SetMarginBottom, box.SetMarginStart, box.SetMarginEnd} {
		f(18)
	}
	return box
}

func comboRow(title string, items ...string) *adw.ComboRow {
	r := adw.NewComboRow()
	r.SetTitle(title)
	r.SetModel(gtk.NewStringList(items))
	return r
}

// ---------------------------------------------------------------- Seite 1: Einstellungen

func (b *batchDialog) settingsPage() *adw.NavigationPage {
	factory := 0
	var names []string
	for _, d := range b.devs {
		if d.NeedsSetup != nil && *d.NeedsSetup {
			factory++
		}
		names = append(names, label(&d))
	}

	box := vbox()
	summary := fmt.Sprintf("%d Geräte: %s", len(b.devs), strings.Join(names, ", "))
	if factory > 0 {
		summary += fmt.Sprintf("\n%d davon im Werkszustand.", factory)
	}
	box.Append(note(summary))

	// Zugang: angemeldete Geräte verwenden ihre geprüften Zugangsdaten
	loggedIn := 0
	for _, d := range b.devs {
		if _, ok := b.w.creds[d.Key()]; ok && d.Unlocked {
			loggedIn++
		}
	}
	needLogin := loggedIn < len(b.devs)
	user := entryRow("Benutzername", "root")
	if b.w.lastUser != "" {
		user.SetText(b.w.lastUser)
	}
	pass := passwordRow("Passwort")
	pass2 := passwordRow("Passwort wiederholen")
	initPw := switchRow("Erstpasswort auf Geräten im Werkszustand setzen", factory > 0)
	initPw.SetSubtitle("Legt den Benutzer oben als Administrator an")
	initPw.SetVisible(factory > 0)
	updPass2 := func() { pass2.SetVisible(initPw.Active()) }
	initPw.NotifyProperty("active", updPass2)
	updPass2()
	box.Append(heading("Zugang"))
	switch {
	case !needLogin:
		box.Append(note("Alle Geräte sind angemeldet – ihre gespeicherten Zugangsdaten werden verwendet."))
	case loggedIn > 0:
		box.Append(note(fmt.Sprintf("%d von %d Geräten sind angemeldet und verwenden ihre gespeicherten "+
			"Zugangsdaten. Benutzer und Passwort unten gelten für die übrigen %d.",
			loggedIn, len(b.devs), len(b.devs)-loggedIn)))
	}
	if needLogin {
		box.Append(boxedList(user, pass, pass2, initPw))
	}

	// Netzwerk
	netMode := comboRow("IPv4", "nicht ändern", "per DHCP", "statisch, fortlaufend")
	start := entryRow("Start-IP-Adresse", "")
	mask := entryRow("Subnetzmaske / Präfix", "255.255.255.0")
	gw := entryRow("Standard-Gateway", "")
	dns1, dns2 := entryRow("DNS-Server 1 (leer = nicht ändern)", ""), entryRow("DNS-Server 2", "")
	order := comboRow("Reihenfolge der Adressvergabe", "nach Seriennummer", "nach aktueller IP", "nach Name")
	csvRow := adw.NewActionRow()
	csvRow.SetTitle("Zuordnung aus CSV-Datei")
	csvRow.SetSubtitle("Spalten: Seriennummer; IP; Hostname – überschreibt Bereich und Muster")
	csvBtn := gtk.NewButtonWithLabel("Laden …")
	csvBtn.SetVAlign(gtk.AlignCenter)
	csvClear := gtk.NewButtonFromIconName("edit-clear-symbolic")
	csvClear.SetVAlign(gtk.AlignCenter)
	csvClear.SetTooltipText("CSV-Plan verwerfen")
	csvClear.AddCSSClass("flat")
	csvClear.SetVisible(false)
	csvRow.AddSuffix(csvBtn)
	csvRow.AddSuffix(csvClear)
	updNet := func() {
		m := netMode.Selected()
		start.SetSensitive(m == 2)
		mask.SetSensitive(m == 2)
		gw.SetSensitive(m == 2)
		order.SetSensitive(m == 2)
		dns1.SetSensitive(m != 0)
		dns2.SetSensitive(m != 0)
	}
	netMode.NotifyProperty("selected", updNet)
	updNet()
	box.Append(heading("Netzwerk"))
	box.Append(boxedList(netMode, start, mask, gw, dns1, dns2, order, csvRow))

	// Hostname
	hostOn := switchRow("Hostnamen setzen", false)
	pattern := entryRow("Name", "kamera-##")
	first := adw.NewSpinRowWithRange(0, 9999, 1)
	first.SetTitle("Startwert für #")
	first.SetValue(1)
	insert := func(ph string) {
		r := []rune(pattern.Text())
		pos := pattern.Position()
		if pos < 0 || pos > len(r) {
			pos = len(r)
		}
		pattern.SetText(string(r[:pos]) + ph + string(r[pos:]))
		pattern.GrabFocus()
		pattern.SetPosition(pos + len([]rune(ph)))
	}
	for _, p := range []struct{ ph, tip string }{
		{"#", "Fortlaufende Nummer einfügen"},
		{"@", "Letzte Zahl der IP-Adresse einfügen"},
	} {
		btn := gtk.NewButtonWithLabel(p.ph)
		btn.SetTooltipText(p.tip)
		btn.AddCSSClass("flat")
		btn.SetVAlign(gtk.AlignCenter)
		ph := p.ph
		btn.ConnectClicked(func() { insert(ph) })
		pattern.AddSuffix(btn)
	}
	hostHelp := note("#  fortlaufende Nummer – ## ergibt 01, 02 …, ### ergibt 001, 002 …\n" +
		"@  letzte Zahl der IP-Adresse – bei 10.0.0.20 also 20")

	// Live-Beispiel: so heißen die ausgewählten Geräte nachher
	exampleList := gtk.NewListBox()
	exampleList.SetSelectionMode(gtk.SelectionNone)
	exampleList.AddCSSClass("boxed-list")
	exampleHead := heading("So werden die Geräte heißen")
	updExample := func() {
		for c := exampleList.FirstChild(); c != nil; c = exampleList.FirstChild() {
			exampleList.Remove(c)
		}
		s := axis.BatchSettings{
			HostnamePattern: strings.TrimSpace(pattern.Text()),
			FirstNumber:     int(first.Value()),
			Order:           []string{"serial", "ip", "name"}[order.Selected()],
		}
		if netMode.Selected() == 2 {
			if p, err := axis.ParsePrefix(mask.Text()); err == nil {
				s.SetNetwork, s.StartIP, s.Prefix, s.Gateway = true, strings.TrimSpace(start.Text()), p, strings.TrimSpace(gw.Text())
			}
		}
		as, err := axis.PlanBatch(b.devs, s, b.all)
		if err != nil { // IP-Bereich (noch) unvollständig → ohne neue IPs
			s.SetNetwork = false
			as, _ = axis.PlanBatch(b.devs, s, b.all)
		}
		count := map[string]int{}
		for _, a := range as {
			count[a.Hostname]++
		}
		const maxShown = 6
		for i, a := range as {
			if i == maxShown {
				more := adw.NewActionRow()
				more.SetTitle(fmt.Sprintf("… und %d weitere", len(as)-maxShown))
				more.AddCSSClass("dim-label")
				exampleList.Append(more)
				break
			}
			ip := a.IP
			if ip == "" {
				ip = a.Device.IP
			}
			r := adw.NewActionRow()
			r.SetUseMarkup(false)
			h := a.Hostname
			if h == "" {
				h = "(leer)"
			}
			r.SetTitle(h)
			r.AddCSSClass("property")
			sub := label(&a.Device) + "  ·  " + ip
			var problem string
			switch {
			case !axis.ValidHostname(a.Hostname):
				problem = "Ungültig: nur a–z, 0–9 und „-“, höchstens 63 Zeichen, nicht mit „-“ beginnen oder enden"
			case count[a.Hostname] > 1:
				problem = "Kommt mehrfach vor – # oder @ in den Namen aufnehmen"
			}
			icon := gtk.NewImageFromIconName("object-select-symbolic")
			icon.AddCSSClass("success")
			if problem != "" {
				sub += "\n" + problem
				icon.SetFromIconName("dialog-warning-symbolic")
				icon.RemoveCSSClass("success")
				icon.AddCSSClass("error")
			}
			r.SetSubtitle(sub)
			r.AddPrefix(icon)
			exampleList.Append(r)
		}
	}
	updHostOn := func() {
		on := hostOn.Active()
		for _, w := range []gtk.Widgetter{pattern, first, hostHelp} {
			gtk.BaseWidget(w).SetSensitive(on)
		}
		exampleHead.SetVisible(on)
		exampleList.SetVisible(on)
		if on {
			updExample()
		}
	}
	hostOn.NotifyProperty("active", updHostOn)
	refreshExample := func() {
		if hostOn.Active() {
			updExample()
		}
	}
	pattern.ConnectChanged(refreshExample)
	first.NotifyProperty("value", refreshExample)
	for _, e := range []*adw.EntryRow{start, mask, gw} {
		e.ConnectChanged(refreshExample)
	}
	netMode.NotifyProperty("selected", refreshExample)
	order.NotifyProperty("selected", refreshExample)
	updHostOn()

	box.Append(heading("Hostname"))
	box.Append(boxedList(hostOn, pattern, first))
	box.Append(hostHelp)
	box.Append(exampleHead)
	box.Append(exampleList)

	// Zeit
	tzOn := switchRow("Zeitzone setzen", false)
	tz := entryRow("Zeitzone (IANA)", localTZ())
	tz.SetSensitive(false)
	tzOn.NotifyProperty("active", func() { tz.SetSensitive(tzOn.Active()) })
	ntpMode := comboRow("NTP", "nicht ändern", "aus", "Server per DHCP", "diese Server")
	ntpServers := entryRow("NTP-Server (kommagetrennt)", "pool.ntp.org")
	updNTP := func() { ntpServers.SetSensitive(ntpMode.Selected() == 3) }
	ntpMode.NotifyProperty("selected", updNTP)
	updNTP()
	box.Append(heading("Datum & Zeit"))
	box.Append(boxedList(tzOn, tz, ntpMode, ntpServers))

	// CSV laden
	csvBtn.ConnectClicked(func() {
		fd := gtk.NewFileDialog()
		fd.SetTitle("CSV-Plan laden")
		filter := gtk.NewFileFilter()
		filter.SetName("CSV-Dateien")
		filter.AddPattern("*.csv")
		filter.AddPattern("*.CSV")
		filter.AddPattern("*.txt")
		filters := gio.NewListStore(gtk.GTypeFileFilter)
		filters.Append(filter.Object)
		fd.SetFilters(filters)
		fd.Open(context.Background(), &b.w.Window, func(res gio.AsyncResulter) {
			f, err := fd.OpenFinish(res)
			if err != nil || f == nil {
				return
			}
			data, err := os.ReadFile(f.Path())
			if err != nil {
				b.toast(err.Error())
				return
			}
			rows, err := axis.ParsePlanCSV(bytes.NewReader(data))
			if err != nil {
				b.toast(err.Error())
				return
			}
			b.plan, b.planName = rows, filepath.Base(f.Path())
			csvRow.SetSubtitle(fmt.Sprintf("%s – %d Zeilen", b.planName, len(rows)))
			csvClear.SetVisible(true)
			for _, r := range rows {
				if r.IP != "" {
					netMode.SetSelected(2)
					if strings.TrimSpace(start.Text()) == "" {
						start.SetText(r.IP) // nur für Subnetz und Prüfung
					}
					break
				}
			}
			for _, r := range rows {
				if r.Hostname != "" {
					hostOn.SetActive(false) // Hostnamen kommen aus der Datei
					break
				}
			}
		})
	})
	csvClear.ConnectClicked(func() {
		b.plan, b.planName = nil, ""
		csvRow.SetSubtitle("Spalten: Seriennummer; IP; Hostname – überschreibt Bereich und Muster")
		csvClear.SetVisible(false)
	})

	next := gtk.NewButtonWithLabel("Weiter")
	next.AddCSSClass("suggested-action")
	cancel := gtk.NewButtonWithLabel("Abbrechen")
	cancel.ConnectClicked(func() { b.dlg.Close() })
	next.ConnectClicked(func() {
		s := axis.BatchSettings{
			User:         strings.TrimSpace(user.Text()),
			Password:     pass.Text(),
			InitPassword: initPw.Active(),
			Order:        []string{"serial", "ip", "name"}[order.Selected()],
		}
		if !needLogin {
			s.User, s.Password, s.InitPassword = "", "", false
		}
		switch {
		case needLogin && (s.User == "" || s.Password == ""):
			b.toast("Benutzername und Passwort erforderlich")
			return
		case s.InitPassword && pass2.Text() != s.Password:
			b.toast("Passwörter stimmen nicht überein")
			return
		case s.InitPassword && len(s.Password) < 8:
			b.toast("Bitte mindestens 8 Zeichen für das Passwort verwenden")
			return
		}
		switch netMode.Selected() {
		case 1:
			s.SetNetwork, s.DHCP = true, true
		case 2:
			p, err := axis.ParsePrefix(mask.Text())
			if err != nil {
				b.toast(err.Error())
				return
			}
			s.SetNetwork, s.StartIP, s.Prefix, s.Gateway = true, strings.TrimSpace(start.Text()), p, strings.TrimSpace(gw.Text())
		}
		if s.SetNetwork {
			for _, r := range []*adw.EntryRow{dns1, dns2} {
				if v := strings.TrimSpace(r.Text()); v != "" {
					s.DNS = append(s.DNS, v)
				}
			}
		}
		if hostOn.Active() {
			s.HostnamePattern, s.FirstNumber = strings.TrimSpace(pattern.Text()), int(first.Value())
		}
		if tzOn.Active() {
			s.TimeZone = strings.TrimSpace(tz.Text())
		}
		switch ntpMode.Selected() {
		case 1:
			s.SetNTP, s.NTP = true, axis.NTPConfig{Enabled: false}
		case 2:
			s.SetNTP, s.NTP = true, axis.NTPConfig{Enabled: true, FromDHCP: true}
		case 3:
			s.SetNTP, s.NTP = true, axis.NTPConfig{Enabled: true}
			for _, v := range strings.Split(ntpServers.Text(), ",") {
				if v = strings.TrimSpace(v); v != "" {
					s.NTP.Servers = append(s.NTP.Servers, v)
				}
			}
		}
		if !s.SetNetwork && s.HostnamePattern == "" && s.TimeZone == "" && !s.SetNTP && b.plan == nil &&
			!(s.InitPassword && factory > 0) {
			b.toast("Es ist nichts zum Ändern ausgewählt")
			return
		}
		as, err := axis.PlanBatch(b.devs, s, b.all)
		if err != nil {
			b.toast(err.Error())
			return
		}
		if b.plan != nil {
			if missing := axis.ApplyPlan(as, b.plan); len(missing) > 0 {
				b.toast(fmt.Sprintf("%d Zeile(n) der CSV passen zu keinem ausgewählten Gerät", len(missing)))
			}
			axis.CheckAssignments(as, s, b.all)
		}
		for i := range as {
			if c, ok := b.w.creds[as[i].Device.Key()]; ok && as[i].Device.Unlocked {
				as[i].User, as[i].Password = c[0], c[1]
			}
		}
		b.settings, b.as = s, as
		b.nav.Push(b.previewPage())
	})
	return page("Einstellungen", cancel, next, box)
}

// ---------------------------------------------------------------- Seite 2: Vorschau

func (b *batchDialog) previewPage() *adw.NavigationPage {
	s := b.settings
	box := vbox()
	box.Append(note("Adressen und Hostnamen lassen sich hier noch einzeln ändern. " +
		"Konfiguriert wird erst mit „Konfigurieren“."))

	grid := gtk.NewGrid()
	grid.SetColumnSpacing(12)
	grid.SetRowSpacing(6)
	for i, h := range []string{"Gerät", "IP bisher", "IP neu", "Hostname", ""} {
		l := gtk.NewLabel(h)
		l.SetXAlign(0)
		l.AddCSSClass("heading")
		grid.Attach(l, i, 0, 1, 1)
	}

	apply := gtk.NewButtonWithLabel("Konfigurieren")
	apply.AddCSSClass("suggested-action")

	hints := make([]*gtk.Label, len(b.as))
	refresh := func() {
		axis.CheckAssignments(b.as, s, b.all)
		ok := true
		for i, a := range b.as {
			probs := a.Problems
			if a.IP != "" && a.IP != a.Device.IP && b.inUse[a.IP] {
				probs = append(probs, "Adresse antwortet bereits (belegt?)")
			}
			h := hints[i]
			h.RemoveCSSClass("error")
			h.RemoveCSSClass("success")
			if len(probs) == 0 {
				h.SetText("✓")
				h.SetTooltipText("")
				h.AddCSSClass("success")
			} else {
				ok = false
				h.SetText("⚠ " + strings.Join(probs, "; "))
				h.SetTooltipText(strings.Join(probs, "\n"))
				h.AddCSSClass("error")
			}
		}
		apply.SetSensitive(ok)
	}
	checkIP := func(ip string) {
		if ip == "" {
			return
		}
		if _, done := b.inUse[ip]; done {
			return
		}
		b.inUse[ip] = false
		async(func() (bool, error) {
			return axis.AddressInUse(context.Background(), ip), nil
		}, func(used bool) {
			b.inUse[ip] = used
			refresh()
		}, func(error) {})
	}

	for i := range b.as {
		a := &b.as[i]
		row := i + 1
		dev := gtk.NewLabel(label(&a.Device))
		dev.SetXAlign(0)
		dev.SetEllipsize(pango.EllipsizeEnd)
		dev.SetMaxWidthChars(28)
		dev.SetTooltipText(a.Device.Serial + " · " + a.Device.Model)
		old := gtk.NewLabel(a.Device.IP)
		old.SetXAlign(0)
		old.AddCSSClass("dim-label")

		ip := gtk.NewEntry()
		ip.SetText(a.IP)
		ip.SetWidthChars(15)
		ip.SetSensitive(s.SetNetwork && !s.DHCP)
		if s.SetNetwork && s.DHCP {
			ip.SetText("DHCP")
		}
		host := gtk.NewEntry()
		host.SetText(a.Hostname)
		host.SetPlaceholderText("unverändert")
		host.SetWidthChars(16)

		hint := gtk.NewLabel("")
		hint.SetXAlign(0)
		hint.SetWrap(true)
		hint.SetHExpand(true)
		hints[i] = hint

		ip.ConnectChanged(func() {
			a.IP = strings.TrimSpace(ip.Text())
			refresh()
			checkIP(a.IP)
		})
		host.ConnectChanged(func() {
			a.Hostname = strings.ToLower(strings.TrimSpace(host.Text()))
			refresh()
		})

		grid.Attach(dev, 0, row, 1, 1)
		grid.Attach(old, 1, row, 1, 1)
		grid.Attach(ip, 2, row, 1, 1)
		grid.Attach(host, 3, row, 1, 1)
		grid.Attach(hint, 4, row, 1, 1)
	}
	box.Append(grid)

	var steps []string
	if s.InitPassword {
		steps = append(steps, "Erstpasswort (Werkszustand)")
	}
	if s.TimeZone != "" {
		steps = append(steps, "Zeitzone "+s.TimeZone)
	}
	if s.SetNTP {
		switch {
		case !s.NTP.Enabled:
			steps = append(steps, "NTP aus")
		case s.NTP.FromDHCP:
			steps = append(steps, "NTP per DHCP")
		default:
			steps = append(steps, "NTP "+strings.Join(s.NTP.Servers, ", "))
		}
	}
	if s.SetNetwork && s.DHCP {
		steps = append(steps, "IPv4 per DHCP")
	} else if s.SetNetwork {
		steps = append(steps, fmt.Sprintf("statische IPs /%d, Gateway %s", s.Prefix, dashIfEmpty(s.Gateway)))
	}
	if len(s.DNS) > 0 {
		steps = append(steps, "DNS "+strings.Join(s.DNS, ", "))
	}
	box.Append(heading("Wird auf jedem Gerät gesetzt"))
	box.Append(note("• " + strings.Join(append(steps, "Hostname laut Tabelle (sofern angegeben)"), "\n• ")))

	refresh()
	for _, a := range b.as {
		checkIP(a.IP)
	}
	apply.ConnectClicked(func() {
		run := b.runPage()
		b.nav.Push(run)
	})
	return page("Vorschau", nil, apply, box)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// ---------------------------------------------------------------- Seite 3: Ausführung

func (b *batchDialog) runPage() *adw.NavigationPage {
	box := vbox()
	status := gtk.NewLabel("Konfiguriere …")
	status.SetXAlign(0)
	status.AddCSSClass("title-4")
	box.Append(status)

	type rowWidgets struct {
		row     *adw.ActionRow
		spinner *gtk.Spinner
		icon    *gtk.Image
	}
	rows := make([]rowWidgets, len(b.as))
	list := gtk.NewListBox()
	list.SetSelectionMode(gtk.SelectionNone)
	list.AddCSSClass("boxed-list")
	for i, a := range b.as {
		r := adw.NewActionRow()
		r.SetTitle(label(&a.Device))
		target := a.IP
		if target == "" {
			target = a.Device.IP
		}
		r.SetSubtitle(fmt.Sprintf("%s → %s · wartet", a.Device.IP, target))
		sp := gtk.NewSpinner()
		icon := gtk.NewImage()
		icon.SetVisible(false)
		r.AddSuffix(sp)
		r.AddSuffix(icon)
		list.Append(r)
		rows[i] = rowWidgets{r, sp, icon}
	}
	box.Append(list)

	ctx, cancel := context.WithCancel(context.Background())
	stop := gtk.NewButtonWithLabel("Abbrechen")
	stop.SetTooltipText("Nach dem aktuellen Gerät anhalten")
	stop.ConnectClicked(func() {
		cancel()
		stop.SetSensitive(false)
		status.SetText("Wird nach dem aktuellen Gerät angehalten …")
	})
	saveLog := gtk.NewButtonWithLabel("Protokoll speichern …")
	saveLog.SetVisible(false)
	closeBtn := gtk.NewButtonWithLabel("Schließen")
	closeBtn.AddCSSClass("suggested-action")
	closeBtn.SetVisible(false)
	end := gtk.NewBox(gtk.OrientationHorizontal, 6)
	end.Append(saveLog)
	end.Append(stop)
	end.Append(closeBtn)

	pg := page("Ausführung", nil, end, box)
	pg.SetCanPop(false)
	b.dlg.SetCanClose(false)

	as, s := b.as, b.settings
	go func() {
		res := axis.RunBatch(ctx, as, s, func(e axis.BatchEvent) {
			ui(func() {
				r := rows[e.Index]
				a := as[e.Index]
				prefix := a.Device.IP + " → " + dashIfEmpty(a.IP)
				if a.IP == "" {
					prefix = a.Device.IP
				}
				switch {
				case !e.Done:
					r.spinner.Start()
					r.row.SetSubtitle(prefix + " · " + e.Step + " …")
				default:
					r.spinner.Stop()
					r.spinner.SetVisible(false)
					r.icon.SetVisible(true)
					if e.Err != nil {
						r.icon.SetFromIconName("dialog-error-symbolic")
						r.icon.AddCSSClass("error")
						r.row.SetSubtitle(prefix + " · " + e.Err.Error())
					} else {
						r.icon.SetFromIconName("object-select-symbolic")
						r.icon.AddCSSClass("success")
						r.row.SetSubtitle(prefix + " · fertig")
					}
				}
			})
		})
		ui(func() {
			cancel()
			failed := 0
			for _, r := range res {
				switch {
				case r.Err == nil:
					if r.Hostname != "" {
						if dev := b.w.reg.Get(r.Device.Key()); dev != nil {
							b.w.onDevice(context.Background(), axis.Device{Serial: dev.Serial, IP: dev.IP,
								Port: dev.Port, Online: true, Hostname: r.Hostname, HostnameSet: true})
						}
					}
					if r.User == "" {
						b.w.creds[r.Device.Key()] = [2]string{s.User, s.Password}
					}
					b.w.setAuth(r.Device.Key(), true)
				case errors.Is(r.Err, axis.ErrAuth):
					failed++
					b.w.setAuth(r.Device.Key(), false)
				default:
					failed++
				}
			}
			if failed == 0 {
				status.SetText(fmt.Sprintf("Alle %d Geräte konfiguriert", len(res)))
			} else {
				status.SetText(fmt.Sprintf("%d von %d Geräten konfiguriert, %d fehlgeschlagen",
					len(res)-failed, len(res), failed))
			}
			stop.SetVisible(false)
			saveLog.SetVisible(true)
			closeBtn.SetVisible(true)
			b.dlg.SetCanClose(true)
			saveLog.ConnectClicked(func() { b.saveLog(res) })
			closeBtn.ConnectClicked(func() { b.dlg.Close() })
			b.dlg.ConnectClosed(func() {
				// IPs haben sich geändert – neu suchen, sobald die Geräte umgestellt sind
				glib.TimeoutSecondsAdd(5, func() { b.w.rescan() })
			})
		})
	}()
	return pg
}

func (b *batchDialog) saveLog(res []axis.BatchResult) {
	fd := gtk.NewFileDialog()
	fd.SetTitle("Protokoll speichern")
	fd.SetInitialName("axis-konfiguration-" + time.Now().Format("2006-01-02-1504") + ".csv")
	fd.Save(context.Background(), &b.w.Window, func(r gio.AsyncResulter) {
		f, err := fd.SaveFinish(r)
		if err != nil || f == nil {
			return
		}
		var buf bytes.Buffer
		if err := axis.WriteResultsCSV(&buf, res); err == nil {
			err = os.WriteFile(f.Path(), buf.Bytes(), 0o644)
		}
		if err != nil {
			b.toast("Protokoll nicht gespeichert: " + err.Error())
		} else {
			b.toast("Protokoll gespeichert")
		}
	})
}
