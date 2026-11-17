// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"axis-ip-tool/internal/axis"
)

// ---------------------------------------------------------------- Bausteine

type formDialog struct {
	*adw.Dialog
	apply   *gtk.Button
	content gtk.Widgetter
	spinner *gtk.Spinner
	toasts  *adw.ToastOverlay
	box     *gtk.Box
}

func newFormDialog(title, applyLabel string, width int) *formDialog {
	d := &formDialog{Dialog: adw.NewDialog()}
	d.SetTitle(title)
	d.SetContentWidth(width)

	hb := adw.NewHeaderBar()
	hb.SetShowEndTitleButtons(false)
	hb.SetShowStartTitleButtons(false)
	cancel := gtk.NewButtonWithLabel("Abbrechen")
	cancel.ConnectClicked(func() { d.Close() })
	hb.PackStart(cancel)
	d.apply = gtk.NewButtonWithLabel(applyLabel)
	d.apply.AddCSSClass("suggested-action")
	hb.PackEnd(d.apply)
	d.SetDefaultWidget(d.apply) // Enter in einem Eingabefeld = Übernehmen
	d.spinner = gtk.NewSpinner()
	d.spinner.SetVisible(false)
	hb.PackEnd(d.spinner)

	d.box = gtk.NewBox(gtk.OrientationVertical, 18)
	for _, f := range []func(int){d.box.SetMarginTop, d.box.SetMarginBottom, d.box.SetMarginStart, d.box.SetMarginEnd} {
		f(18)
	}
	scroller := gtk.NewScrolledWindow()
	scroller.SetPropagateNaturalHeight(true)
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(d.box)
	d.toasts = adw.NewToastOverlay()
	d.toasts.SetChild(scroller)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(d.toasts)
	d.SetChild(tv)
	return d
}

func (d *formDialog) add(w gtk.Widgetter) { d.box.Append(w) }

func (d *formDialog) busy(on bool) {
	d.spinner.SetVisible(on)
	if on {
		d.spinner.Start()
	} else {
		d.spinner.Stop()
	}
	d.apply.SetSensitive(!on)
	d.box.SetSensitive(!on)
}

func (d *formDialog) toast(msg string) {
	t := adw.NewToast(msg)
	t.SetTimeout(5)
	d.toasts.AddToast(t)
}

func boxedList(rows ...gtk.Widgetter) *gtk.ListBox {
	lb := gtk.NewListBox()
	lb.SetSelectionMode(gtk.SelectionNone)
	lb.AddCSSClass("boxed-list")
	for _, r := range rows {
		lb.Append(r)
	}
	return lb
}

func heading(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.AddCSSClass("heading")
	return l
}

func note(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.SetWrap(true)
	l.AddCSSClass("dim-label")
	return l
}

func entryRow(title, text string) *adw.EntryRow {
	r := adw.NewEntryRow()
	r.SetTitle(title)
	r.SetText(text)
	r.SetActivatesDefault(true)
	return r
}

func passwordRow(title string) *adw.PasswordEntryRow {
	r := adw.NewPasswordEntryRow()
	r.SetTitle(title)
	r.SetActivatesDefault(true)
	return r
}

func switchRow(title string, active bool) *adw.SwitchRow {
	r := adw.NewSwitchRow()
	r.SetTitle(title)
	r.SetActive(active)
	return r
}

// disableWhenOn graut Zeilen aus, solange der Schalter an ist (z. B. „per DHCP“).
func disableWhenOn(sw *adw.SwitchRow, rows ...gtk.Widgetter) {
	upd := func() {
		for _, r := range rows {
			gtk.BaseWidget(r).SetSensitive(!sw.Active())
		}
	}
	sw.NotifyProperty("active", upd)
	upd()
}

func async[T any](f func() (T, error), done func(T), fail func(error)) {
	go func() {
		v, err := f()
		glib.IdleAdd(func() {
			if err != nil {
				fail(err)
			} else {
				done(v)
			}
		})
	}()
}

func label(d *axis.Device) string {
	if d.Name != "" {
		return d.Name
	}
	return d.IP
}

// ---------------------------------------------------------------- Anmeldung

// withLogin führt action mit gespeicherten Zugangsdaten aus und fragt bei
// Bedarf (erneut) nach Benutzer und Passwort.
func withLogin[T any](w *window, d *axis.Device, what string,
	action func(context.Context, *axis.Client) (T, error), done func(T)) {

	if d.NeedsSetup != nil && *d.NeedsSetup {
		// Werkszustand: Anmelden ist unmöglich, erst einen Benutzer anlegen
		w.toast(label(d) + ": noch kein Benutzer angelegt – bitte zuerst ein Passwort setzen")
		w.selectKey(d.Key())
		w.onPassword()
		return
	}
	var attempt func(user, pass string, forAll bool)
	attempt = func(user, pass string, forAll bool) {
		key := d.Key()
		ip, port := d.IP, d.Port
		async(func() (T, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return action(ctx, axis.NewClient(ip, port, user, pass))
		}, func(v T) {
			w.creds[key] = [2]string{user, pass}
			done(v)
			w.refreshInfo(key)
			if forAll {
				w.unlockOthers(key, user, pass)
			}
		}, func(err error) {
			switch {
			case errors.Is(err, axis.ErrNoUser):
				delete(w.creds, key)
				if dev := w.reg.Get(key); dev != nil {
					yes := true
					w.onDevice(context.Background(), axis.Device{Serial: dev.Serial, IP: dev.IP,
						Port: dev.Port, Online: true, NeedsSetup: &yes})
				}
				w.toast(fmt.Sprintf("%s: %v", label(d), err))
			case errors.Is(err, axis.ErrAuth):
				w.setAuth(key, false)
				w.askLogin(d, err.Error(), attempt)
			default:
				w.toast(fmt.Sprintf("%s: %v", what, err))
			}
		})
	}
	if c, ok := w.creds[d.Key()]; ok {
		attempt(c[0], c[1], false)
	} else {
		w.askLogin(d, "", attempt)
	}
}

func (w *window) askLogin(d *axis.Device, errMsg string, cb func(user, pass string, forAll bool)) {
	body := fmt.Sprintf("%s (%s)", label(d), d.IP)
	if errMsg != "" {
		body += "\n\n" + errMsg
	}
	dlg := adw.NewAlertDialog("Anmeldung erforderlich", body)
	user := entryRow("Benutzername", "root")
	if c, ok := w.creds[d.Key()]; ok {
		user.SetText(c[0])
	} else if w.lastUser != "" {
		user.SetText(w.lastUser)
	}
	pass := passwordRow("Passwort")
	pass.SetActivatesDefault(true)
	extra := gtk.NewBox(gtk.OrientationVertical, 12)
	extra.Append(boxedList(user, pass))
	forAll := gtk.NewCheckButtonWithLabel("")
	if n := len(w.loginCandidates(d.Key())); n > 0 {
		forAll.SetLabel(fmt.Sprintf("Diese Zugangsdaten auch bei allen anderen nicht angemeldeten Geräten verwenden (%d)", n))
		forAll.SetActive(w.lastForAll)
		if l, ok := forAll.Child().(*gtk.Label); ok {
			l.SetWrap(true)
			l.SetXAlign(0)
		}
		extra.Append(forAll)
	}
	dlg.SetExtraChild(extra)
	dlg.AddResponse("cancel", "Abbrechen")
	dlg.AddResponse("ok", "Anmelden")
	dlg.SetResponseAppearance("ok", adw.ResponseSuggested)
	dlg.SetDefaultResponse("ok")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(r string) {
		if r == "ok" {
			all := forAll.Parent() != nil && forAll.Active()
			w.lastUser, w.lastForAll = strings.TrimSpace(user.Text()), all
			cb(strings.TrimSpace(user.Text()), pass.Text(), all)
		}
	})
	dlg.Present(w)
	pass.GrabFocus()
}

// loginCandidates: alle anderen erreichbaren Geräte ohne geprüfte Anmeldung
// (gesperrt, nicht angemeldet oder Passwort falsch), aber nicht im Werkszustand.
func (w *window) loginCandidates(except string) []string {
	var keys []string
	for _, d := range w.reg.All() {
		if d.Key() == except || !d.Online || d.IP == "" || d.Unlocked ||
			(d.NeedsSetup != nil && *d.NeedsSetup) {
			continue
		}
		keys = append(keys, d.Key())
	}
	return keys
}

// unlockOthers probiert die Zugangsdaten bei allen übrigen Kandidaten und meldet das Ergebnis.
func (w *window) unlockOthers(except, user, pass string) {
	keys := w.loginCandidates(except)
	if len(keys) == 0 {
		return
	}
	reg := w.reg
	ok, done := 0, 0
	finish := func() {
		done++
		if done < len(keys) || reg != w.reg {
			return
		}
		switch ok {
		case len(keys):
			w.toast(fmt.Sprintf("Zugangsdaten bei allen %d weiteren Geräten gültig", ok))
		case 0:
			w.toast("Zugangsdaten bei keinem weiteren Gerät gültig")
		default:
			w.toast(fmt.Sprintf("Zugangsdaten bei %d von %d weiteren Geräten gültig", ok, len(keys)))
		}
	}
	for _, key := range keys {
		d := reg.Get(key)
		ip, port := d.IP, d.Port
		w.infoPending[key] = true
		w.rerender(key)
		async(func() (axis.Device, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := axis.NewClient(ip, port, user, pass)
			return loginInfo(ctx, c)
		}, func(info axis.Device) {
			delete(w.infoPending, key)
			if dev := reg.Get(key); dev != nil && reg == w.reg {
				ok++
				w.creds[key] = [2]string{user, pass}
				w.onDevice(context.Background(), asUpdate(info, dev))
			}
			finish()
		}, func(error) { // andere Zugangsdaten: Status bleibt, wie er ist
			delete(w.infoPending, key)
			if reg == w.reg {
				w.rerender(key)
			}
			finish()
		})
	}
}

// loginInfo prüft die Zugangsdaten und liest Modell, Firmware und den konfigurierten
// Hostnamen. Das Ergebnis eignet sich direkt für onDevice (Serial/IP ergänzen).
func loginInfo(ctx context.Context, c *axis.Client) (axis.Device, error) {
	if err := c.CheckLogin(ctx); err != nil {
		return axis.Device{}, err
	}
	info, err := c.BasicInfo(ctx)
	if err != nil {
		return axis.Device{}, err
	}
	out := axis.Device{Model: info.Model, Firmware: info.Firmware, Unlocked: true, Online: true}
	if n, err := c.GetNetwork(ctx); err == nil && n.Hostname != "" {
		out.Hostname, out.HostnameSet = strings.ToLower(n.Hostname), true
	}
	return out, nil
}

// asUpdate ergänzt ein loginInfo-Ergebnis um die Identität des Geräts.
func asUpdate(info axis.Device, dev *axis.Device) axis.Device {
	info.Serial, info.IP, info.Port = dev.Serial, dev.IP, dev.Port
	return info
}

// ---------------------------------------------------------------- Aktionen

func (w *window) onBrowser() {
	d := w.selected()
	if d == nil {
		return
	}
	gtk.NewURILauncher(d.WebURL()).Launch(context.Background(), &w.Window, nil)
}

func (w *window) onPassword() {
	d := w.selected()
	if d == nil {
		return
	}
	user := entryRow("Benutzername", "root")
	p1 := passwordRow("Passwort")
	p2 := passwordRow("Passwort wiederholen")
	dlg := newFormDialog("Passwort – "+label(d), "Setzen", 460)
	if d.NeedsSetup != nil && !*d.NeedsSetup {
		dlg.add(note("Auf diesem Gerät ist bereits ein Benutzer eingerichtet. Zum Ändern des " +
			"Passworts bitte die Weboberfläche verwenden oder das Gerät zurücksetzen."))
	} else {
		dlg.add(note("Legt den ersten Administrator auf einem Gerät im Werkszustand an. " +
			"Ab AXIS OS 10 ist auch ein anderer Benutzername als „root“ möglich."))
	}
	if d.HTTPS != nil && !*d.HTTPS {
		warn := note("⚠ Dieses Gerät ist nur per HTTP erreichbar. Das Passwort wird unverschlüsselt " +
			"übertragen und kann im lokalen Netz mitgelesen werden.")
		warn.RemoveCSSClass("dim-label")
		warn.AddCSSClass("warning")
		dlg.add(warn)
	}
	dlg.add(boxedList(user, p1, p2))
	dlg.SetFocus(p1)
	key, ip, port := d.Key(), d.IP, d.Port
	dlg.apply.ConnectClicked(func() {
		u, a, b := strings.TrimSpace(user.Text()), p1.Text(), p2.Text()
		switch {
		case u == "" || a == "":
			dlg.toast("Benutzername und Passwort erforderlich")
			return
		case a != b:
			dlg.toast("Passwörter stimmen nicht überein")
			return
		case len(a) < 8:
			dlg.toast("Bitte mindestens 8 Zeichen verwenden")
			return
		}
		dlg.busy(true)
		async(func() (struct{}, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return struct{}{}, axis.NewClient(ip, port, "", "").CreateInitialUser(ctx, u, a)
		}, func(struct{}) {
			w.creds[key] = [2]string{u, a}
			if dev := w.reg.Get(key); dev != nil {
				f := false
				dev.NeedsSetup = &f
				w.onDevice(context.Background(), *dev)
			}
			w.toast(fmt.Sprintf("Benutzer „%s“ auf %s angelegt", u, ip))
			dlg.Close()
		}, func(err error) {
			dlg.busy(false)
			dlg.toast(err.Error())
		})
	})
	dlg.Present(w)
}

func (w *window) onNetwork() {
	d := w.selected()
	if d == nil || d.IP == "" {
		return
	}
	if d.NeedsSetup != nil && *d.NeedsSetup {
		w.toast("Zuerst ein Passwort setzen")
		w.onPassword()
		return
	}
	withLogin(w, d, "Netzwerk", func(ctx context.Context, c *axis.Client) (axis.NetworkConfig, error) {
		return c.GetNetwork(ctx)
	}, func(cfg axis.NetworkConfig) { w.networkDialog(d, cfg) })
}

func (w *window) networkDialog(d *axis.Device, cfg axis.NetworkConfig) {
	dhcp := switchRow("IPv4 per DHCP beziehen", cfg.DHCP)
	addr := entryRow("IP-Adresse", cfg.Address)
	mask := entryRow("Subnetzmaske / Präfix", axis.PrefixToMask(cfg.Prefix))
	gw := entryRow("Standard-Gateway", cfg.Gateway)
	disableWhenOn(dhcp, addr, mask, gw)

	hostDHCP := switchRow("Hostname per DHCP", cfg.HostnameFromDHCP)
	host := entryRow("Hostname", cfg.Hostname)
	disableWhenOn(hostDHCP, host)

	dnsDHCP := switchRow("DNS-Server per DHCP", cfg.DNSFromDHCP)
	dns := []string{"", ""}
	copy(dns, cfg.DNS)
	dns1, dns2 := entryRow("DNS-Server 1", dns[0]), entryRow("DNS-Server 2", dns[1])
	disableWhenOn(dnsDHCP, dns1, dns2)

	dlg := newFormDialog("Netzwerk – "+label(d), "Übernehmen", 500)
	dlg.add(heading("IPv4"))
	dlg.add(boxedList(dhcp, addr, mask, gw))
	dlg.add(heading("Hostname"))
	dlg.add(boxedList(hostDHCP, host))
	dlg.add(heading("DNS"))
	dlg.add(boxedList(dnsDHCP, dns1, dns2))
	if cfg.Legacy {
		dlg.add(note("Ältere Firmware: Konfiguration über param.cgi"))
	}

	oldIP, key := d.IP, d.Key()
	dlg.apply.ConnectClicked(func() {
		n := cfg
		n.DHCP = dhcp.Active()
		n.HostnameFromDHCP, n.Hostname = hostDHCP.Active(), strings.TrimSpace(host.Text())
		n.DNSFromDHCP, n.DNS = dnsDHCP.Active(), nil
		if !n.DHCP {
			n.Address, n.Gateway = strings.TrimSpace(addr.Text()), strings.TrimSpace(gw.Text())
			p, err := axis.ParsePrefix(mask.Text())
			if err != nil {
				dlg.toast(err.Error())
				return
			}
			n.Prefix = p
		}
		if !n.DNSFromDHCP {
			for _, r := range []*adw.EntryRow{dns1, dns2} {
				if s := strings.TrimSpace(r.Text()); s != "" {
					n.DNS = append(n.DNS, s)
				}
			}
		}
		if err := n.Validate(); err != nil {
			dlg.toast(err.Error())
			return
		}
		creds := w.creds[key]
		dlg.busy(true)
		async(func() (struct{}, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return struct{}{}, axis.NewClient(oldIP, d.Port, creds[0], creds[1]).SetNetwork(ctx, n)
		}, func(struct{}) {
			dlg.Close()
			switch {
			case !n.DHCP && n.Address != oldIP:
				w.creds[key] = creds
				w.toast("Neue Adresse " + n.Address + " gesetzt – Suche wird aktualisiert")
				glib.TimeoutSecondsAdd(5, func() { w.rescan() })
			case n.DHCP != cfg.DHCP:
				w.toast("Auf DHCP umgestellt – Suche wird aktualisiert")
				glib.TimeoutSecondsAdd(8, func() { w.rescan() })
			default:
				w.toast("Netzwerkeinstellungen gespeichert")
			}
			if dev := w.reg.Get(key); dev != nil && !n.HostnameFromDHCP && n.Hostname != "" {
				w.onDevice(context.Background(), axis.Device{Serial: dev.Serial, IP: dev.IP, Port: dev.Port,
					Online: true, Hostname: strings.ToLower(n.Hostname), HostnameSet: true})
			}
		}, func(err error) {
			dlg.busy(false)
			dlg.toast(err.Error())
		})
	})
	dlg.Present(w)
}

// ntpState ist das Ergebnis der NTP-Abfrage; Err != nil heißt „NTP nicht einstellbar“.
type ntpState struct {
	Cfg axis.NTPConfig
	Err error
}

func (w *window) onTime() {
	d := w.selected()
	if d == nil {
		return
	}
	if d.NeedsSetup != nil && *d.NeedsSetup {
		w.toast("Zuerst ein Passwort setzen")
		w.onPassword()
		return
	}
	withLogin(w, d, "Datum & Zeit", func(ctx context.Context, c *axis.Client) (ntpState, error) {
		cfg, err := c.GetNTP(ctx)
		if errors.Is(err, axis.ErrAuth) {
			return ntpState{}, err
		}
		return ntpState{cfg, err}, nil
	}, func(st ntpState) { w.timeDialog(d, st) })
}

func (w *window) timeDialog(d *axis.Device, st ntpState) {
	cfg := st.Cfg
	tz := entryRow("Zeitzone (IANA, z. B. Europe/Vienna)", localTZ())

	ntpOn := switchRow("Uhrzeit per NTP synchronisieren", cfg.Enabled)
	ntpDHCP := switchRow("NTP-Server per DHCP beziehen", cfg.FromDHCP)
	srv := []string{"", ""}
	copy(srv, cfg.Servers)
	if !cfg.Enabled && srv[0] == "" {
		srv[0] = "pool.ntp.org"
	}
	ntp1, ntp2 := entryRow("NTP-Server 1", srv[0]), entryRow("NTP-Server 2", srv[1])
	manual := switchRow("Uhrzeit jetzt von diesem Rechner übernehmen", !cfg.Enabled)
	manual.SetSubtitle("Nur ohne NTP sinnvoll")

	upd := func() {
		on := ntpOn.Active() && st.Err == nil
		ntpDHCP.SetSensitive(on)
		ntp1.SetSensitive(on && !ntpDHCP.Active())
		ntp2.SetSensitive(on && !ntpDHCP.Active())
		manual.SetSensitive(!on)
	}
	ntpOn.NotifyProperty("active", upd)
	ntpDHCP.NotifyProperty("active", upd)
	upd()

	dlg := newFormDialog("Datum & Zeit – "+label(d), "Übernehmen", 500)
	dlg.add(heading("Zeitzone"))
	dlg.add(boxedList(tz))
	dlg.add(heading("Zeitsynchronisierung (NTP)"))
	if st.Err != nil {
		ntpOn.SetSensitive(false)
		dlg.add(note("NTP-Einstellungen konnten nicht gelesen werden: " + st.Err.Error()))
	} else {
		dlg.add(boxedList(ntpOn, ntpDHCP, ntp1, ntp2))
		var info []string
		if cfg.Enabled && len(cfg.Active) > 0 {
			info = append(info, "Verwendete Server: "+strings.Join(cfg.Active, ", "))
		}
		if cfg.Enabled && cfg.Synced != nil {
			info = append(info, map[bool]string{true: "Uhrzeit ist synchronisiert.",
				false: "Uhrzeit ist noch nicht synchronisiert."}[*cfg.Synced])
		}
		if cfg.Legacy {
			info = append(info, "Ältere Firmware: nur ein NTP-Server über param.cgi.")
		}
		if len(info) > 0 {
			dlg.add(note(strings.Join(info, "\n")))
		}
	}
	dlg.add(heading("Manuell"))
	dlg.add(boxedList(manual))

	key, ip, port := d.Key(), d.IP, d.Port
	dlg.apply.ConnectClicked(func() {
		name := strings.TrimSpace(tz.Text())
		n := cfg
		n.Enabled, n.FromDHCP, n.Servers = ntpOn.Active(), ntpDHCP.Active(), nil
		for _, r := range []*adw.EntryRow{ntp1, ntp2} {
			if s := strings.TrimSpace(r.Text()); s != "" {
				n.Servers = append(n.Servers, s)
			}
		}
		setNTP := st.Err == nil
		syncNow := manual.Active() && !(setNTP && n.Enabled)
		creds := w.creds[key]
		dlg.busy(true)
		async(func() (struct{}, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := axis.NewClient(ip, port, creds[0], creds[1])
			if setNTP {
				if err := c.SetNTP(ctx, n); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, c.SetTime(ctx, name, syncNow)
		}, func(struct{}) {
			dlg.Close()
			w.toast("Datum & Zeit gespeichert")
		}, func(err error) {
			dlg.busy(false)
			dlg.toast(err.Error())
		})
	})
	dlg.Present(w)
}

// ---------------------------------------------------------------- Vorschau

const previewWidth = 1280

func (w *window) onPreview() {
	d := w.selected()
	if d == nil {
		return
	}
	if d.NeedsSetup != nil && *d.NeedsSetup {
		w.toast("Zuerst ein Passwort setzen")
		w.onPassword()
		return
	}
	withLogin(w, d, "Vorschau", func(ctx context.Context, c *axis.Client) ([]byte, error) {
		return c.Snapshot(ctx, previewWidth)
	}, func(img []byte) { w.previewDialog(d, img) })
}

func (w *window) previewDialog(d *axis.Device, first []byte) {
	dlg := adw.NewDialog()
	dlg.SetTitle("Vorschau – " + label(d))
	dlg.SetContentWidth(960)
	dlg.SetContentHeight(620)

	pic := gtk.NewPicture()
	pic.SetContentFit(gtk.ContentFitContain)
	pic.SetCanShrink(true)
	pic.SetVExpand(true)
	pic.SetHExpand(true)

	status := gtk.NewLabel("")
	status.AddCSSClass("dim-label")
	status.SetEllipsize(pango.EllipsizeEnd)

	live := gtk.NewToggleButton()
	live.SetIconName("media-playback-start-symbolic")
	live.SetTooltipText("Automatisch aktualisieren (1 Bild/s)")
	live.SetActive(true)
	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.SetTooltipText("Bild neu laden")

	hb := adw.NewHeaderBar()
	hb.PackStart(live)
	hb.PackStart(refresh)
	bar := gtk.NewActionBar()
	bar.SetCenterWidget(status)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(pic)
	tv.AddBottomBar(bar)
	dlg.SetChild(tv)

	show := func(img []byte) {
		tex, err := gdk.NewTextureFromBytes(glib.NewBytes(img))
		if err != nil {
			status.SetText("Bild nicht lesbar: " + err.Error())
			return
		}
		pic.SetPaintable(tex)
		status.SetText(fmt.Sprintf("%d × %d · Stand %s", tex.Width(), tex.Height(), time.Now().Format("15:04:05")))
	}
	show(first)

	key, ip, port := d.Key(), d.IP, d.Port
	closed, loading := false, false
	fetch := func() {
		if closed || loading {
			return
		}
		loading = true
		creds := w.creds[key]
		async(func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			return axis.NewClient(ip, port, creds[0], creds[1]).Snapshot(ctx, previewWidth)
		}, func(img []byte) {
			loading = false
			if !closed {
				show(img)
			}
		}, func(err error) {
			loading = false
			if !closed {
				status.SetText(err.Error())
			}
		})
	}
	refresh.ConnectClicked(fetch)
	glib.TimeoutAdd(1000, func() bool {
		if closed {
			return false
		}
		if live.Active() {
			fetch()
		}
		return true
	})
	dlg.ConnectClosed(func() { closed = true })
	dlg.Present(w)
}

func (w *window) onRestart() {
	d := w.selected()
	if d == nil {
		return
	}
	dlg := adw.NewAlertDialog("Gerät neu starten?", fmt.Sprintf("%s (%s) wird neu gestartet.", label(d), d.IP))
	dlg.AddResponse("cancel", "Abbrechen")
	dlg.AddResponse("restart", "Neu starten")
	dlg.SetResponseAppearance("restart", adw.ResponseDestructive)
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(r string) {
		if r != "restart" {
			return
		}
		withLogin(w, d, "Neustart", func(ctx context.Context, c *axis.Client) (struct{}, error) {
			return struct{}{}, c.Restart(ctx)
		}, func(struct{}) { w.toast("Neustart ausgelöst") })
	})
	dlg.Present(w)
}

// onReset setzt das Gerät nach Rückfrage auf Werkseinstellungen zurück.
func (w *window) onReset() {
	d := w.selected()
	if d == nil {
		return
	}
	dlg := adw.NewAlertDialog("Auf Werkseinstellungen zurücksetzen?",
		fmt.Sprintf("%s (%s)\n\nAlle Einstellungen, Benutzer und Passwörter werden gelöscht. "+
			"Das Gerät startet neu und muss danach mit einem Erstpasswort neu eingerichtet werden.",
			label(d), d.IP))
	keep := gtk.NewCheckButtonWithLabel("Netzwerkeinstellungen (IP-Adresse) behalten")
	keep.SetActive(true)
	hint := note("")
	hint.SetMarginStart(28)
	updHint := func() {
		if keep.Active() {
			hint.SetText("Das Gerät bleibt unter " + d.IP + " erreichbar.")
		} else {
			hint.SetText("Das Gerät holt sich danach per DHCP eine Adresse, ohne DHCP-Server " +
				"nimmt es 192.168.0.90.")
		}
	}
	keep.ConnectToggled(updHint)
	updHint()
	extra := gtk.NewBox(gtk.OrientationVertical, 6)
	extra.Append(keep)
	extra.Append(hint)
	dlg.SetExtraChild(extra)
	dlg.AddResponse("cancel", "Abbrechen")
	dlg.AddResponse("reset", "Zurücksetzen")
	dlg.SetResponseAppearance("reset", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.SetCloseResponse("cancel")
	key := d.Key()
	dlg.ConnectResponse(func(r string) {
		if r != "reset" {
			return
		}
		keepNet := keep.Active()
		withLogin(w, d, "Zurücksetzen", func(ctx context.Context, c *axis.Client) (struct{}, error) {
			if err := c.CheckLogin(ctx); err != nil { // nicht anonym auslösen lassen
				return struct{}{}, err
			}
			return struct{}{}, c.FactoryReset(ctx, keepNet)
		}, func(struct{}) {
			delete(w.creds, key)
			if dev := w.reg.Get(key); dev != nil {
				yes := true
				w.onDevice(context.Background(), axis.Device{Serial: dev.Serial, IP: dev.IP,
					Port: dev.Port, Online: true, NeedsSetup: &yes})
			}
			if keepNet {
				w.toast(label(d) + " wird zurückgesetzt und startet neu (ca. 1–2 Minuten)")
			} else {
				w.toast(label(d) + " wird zurückgesetzt – danach neu suchen, die IP-Adresse ändert sich")
			}
		})
	})
	dlg.Present(w)
}

func localTZ() string {
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(l, "zoneinfo/"); i >= 0 {
			return l[i+9:]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "Europe/Vienna"
}
