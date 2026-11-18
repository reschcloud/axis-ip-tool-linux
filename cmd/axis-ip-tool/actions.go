// SPDX-License-Identifier: AGPL-3.0-or-later

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

// ---------------------------------------------------------------- Building blocks

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
	cancel := gtk.NewButtonWithLabel("Cancel")
	cancel.ConnectClicked(func() { d.Close() })
	hb.PackStart(cancel)
	d.apply = gtk.NewButtonWithLabel(applyLabel)
	d.apply.AddCSSClass("suggested-action")
	hb.PackEnd(d.apply)
	d.SetDefaultWidget(d.apply) // Enter in an entry = apply
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

// disableWhenOn greys out rows while the switch is on (e.g. "via DHCP").
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

// ---------------------------------------------------------------- Login

// withLogin runs action with stored credentials and asks for user and password
// (again) if necessary.
func withLogin[T any](w *window, d *axis.Device, what string,
	action func(context.Context, *axis.Client) (T, error), done func(T)) {

	if d.NeedsSetup != nil && *d.NeedsSetup {
		// factory state: logging in is impossible, create a user first
		w.toast(label(d) + ": no user created yet – please set a password first")
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
	dlg := adw.NewAlertDialog("Login required", body)
	user := entryRow("User name", "root")
	if c, ok := w.creds[d.Key()]; ok {
		user.SetText(c[0])
	} else if w.lastUser != "" {
		user.SetText(w.lastUser)
	}
	pass := passwordRow("Password")
	pass.SetActivatesDefault(true)
	extra := gtk.NewBox(gtk.OrientationVertical, 12)
	extra.Append(boxedList(user, pass))
	forAll := gtk.NewCheckButtonWithLabel("")
	if n := len(w.loginCandidates(d.Key())); n > 0 {
		forAll.SetLabel(fmt.Sprintf("Use these credentials for all other devices that are not logged in too (%d)", n))
		forAll.SetActive(w.lastForAll)
		if l, ok := forAll.Child().(*gtk.Label); ok {
			l.SetWrap(true)
			l.SetXAlign(0)
		}
		extra.Append(forAll)
	}
	dlg.SetExtraChild(extra)
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("ok", "Log in")
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

// loginCandidates: all other reachable devices without verified login
// (locked, not logged in or wrong password), but not in factory state.
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

// unlockOthers tries the credentials on all remaining candidates and reports the result.
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
			w.toast(fmt.Sprintf("Credentials valid on all %d other devices", ok))
		case 0:
			w.toast("Credentials not valid on any other device")
		default:
			w.toast(fmt.Sprintf("Credentials valid on %d of %d other devices", ok, len(keys)))
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
		}, func(error) { // different credentials: the status stays as it is
			delete(w.infoPending, key)
			if reg == w.reg {
				w.rerender(key)
			}
			finish()
		})
	}
}

// loginInfo verifies the credentials and reads model, firmware and the configured
// hostname. The result is suitable for onDevice directly (add serial/IP).
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

// asUpdate adds the device identity to a loginInfo result.
func asUpdate(info axis.Device, dev *axis.Device) axis.Device {
	info.Serial, info.IP, info.Port = dev.Serial, dev.IP, dev.Port
	return info
}

// ---------------------------------------------------------------- Actions

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
	user := entryRow("User name", "root")
	p1 := passwordRow("Password")
	p2 := passwordRow("Repeat password")
	dlg := newFormDialog("Password – "+label(d), "Set", 460)
	if d.NeedsSetup != nil && !*d.NeedsSetup {
		dlg.add(note("A user is already set up on this device. To change the password, " +
			"please use the web interface or reset the device."))
	} else {
		dlg.add(note("Creates the first administrator on a device in factory state. " +
			"From AXIS OS 10 on, a user name other than \"root\" is possible too."))
	}
	if d.HTTPS != nil && !*d.HTTPS {
		warn := note("⚠ This device is only reachable via HTTP. The password is transmitted " +
			"unencrypted and can be read by others on the local network.")
		warn.RemoveCSSClass("dim-label")
		warn.AddCSSClass("warning")
		dlg.add(warn)
	}
	dlg.add(boxedList(user, p1, p2))
	// power line frequency/mounting position only at commissioning (device without user)
	freq, mount := setupRows()
	if d.NeedsSetup == nil || *d.NeedsSetup {
		dlg.add(heading("Commissioning"))
		dlg.add(boxedList(freq, mount))
	}
	initial := d.NeedsSetup == nil || *d.NeedsSetup
	dlg.SetFocus(p1)
	key, ip, port := d.Key(), d.IP, d.Port
	dlg.apply.ConnectClicked(func() {
		u, a, b := strings.TrimSpace(user.Text()), p1.Text(), p2.Text()
		switch {
		case u == "" || a == "":
			dlg.toast("User name and password required")
			return
		case a != b:
			dlg.toast("Passwords don't match")
			return
		case len(a) < 8:
			dlg.toast("Please use at least 8 characters")
			return
		}
		var setup axis.InitialSetup
		if initial {
			setup = readSetup(freq, mount)
		}
		dlg.busy(true)
		type result struct {
			skipped  []string
			setupErr error
		}
		async(func() (result, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			if err := axis.NewClient(ip, port, "", "").CreateInitialUser(ctx, u, a); err != nil {
				return result{}, err
			}
			// the user exists – only report errors of the image settings
			var r result
			if !setup.Empty() {
				r.skipped, r.setupErr = axis.NewClient(ip, port, u, a).ApplyInitialSetup(ctx, setup)
			}
			return r, nil
		}, func(r result) {
			w.creds[key] = [2]string{u, a}
			if dev := w.reg.Get(key); dev != nil {
				f := false
				dev.NeedsSetup = &f
				w.onDevice(context.Background(), *dev)
			}
			w.toast(fmt.Sprintf("User \"%s\" created on %s%s", u, ip, setupResult(setup, r.skipped, r.setupErr)))
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
		w.toast("Set a password first")
		w.onPassword()
		return
	}
	withLogin(w, d, "Network", func(ctx context.Context, c *axis.Client) (axis.NetworkConfig, error) {
		return c.GetNetwork(ctx)
	}, func(cfg axis.NetworkConfig) { w.networkDialog(d, cfg) })
}

func (w *window) networkDialog(d *axis.Device, cfg axis.NetworkConfig) {
	dhcp := switchRow("Obtain IPv4 via DHCP", cfg.DHCP)
	addr := entryRow("IP address", cfg.Address)
	mask := entryRow("Subnet mask / prefix", axis.PrefixToMask(cfg.Prefix))
	gw := entryRow("Default gateway", cfg.Gateway)
	disableWhenOn(dhcp, addr, mask, gw)

	hostDHCP := switchRow("Hostname via DHCP", cfg.HostnameFromDHCP)
	host := entryRow("Hostname", cfg.Hostname)
	disableWhenOn(hostDHCP, host)

	dnsDHCP := switchRow("DNS servers via DHCP", cfg.DNSFromDHCP)
	dns := []string{"", ""}
	copy(dns, cfg.DNS)
	dns1, dns2 := entryRow("DNS server 1", dns[0]), entryRow("DNS server 2", dns[1])
	disableWhenOn(dnsDHCP, dns1, dns2)

	dlg := newFormDialog("Network – "+label(d), "Apply", 500)
	dlg.add(heading("IPv4"))
	dlg.add(boxedList(dhcp, addr, mask, gw))
	dlg.add(heading("Hostname"))
	dlg.add(boxedList(hostDHCP, host))
	dlg.add(heading("DNS"))
	dlg.add(boxedList(dnsDHCP, dns1, dns2))
	if cfg.Legacy {
		dlg.add(note("Older firmware: configuration via param.cgi"))
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
				w.toast("New address " + n.Address + " set – refreshing discovery")
				glib.TimeoutSecondsAdd(5, func() { w.rescan() })
			case n.DHCP != cfg.DHCP:
				w.toast("Switched to DHCP – refreshing discovery")
				glib.TimeoutSecondsAdd(8, func() { w.rescan() })
			default:
				w.toast("Network settings saved")
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

// ntpState is the result of the NTP query; Err != nil means "NTP not configurable".
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
		w.toast("Set a password first")
		w.onPassword()
		return
	}
	withLogin(w, d, "Date & time", func(ctx context.Context, c *axis.Client) (ntpState, error) {
		cfg, err := c.GetNTP(ctx)
		if errors.Is(err, axis.ErrAuth) {
			return ntpState{}, err
		}
		return ntpState{cfg, err}, nil
	}, func(st ntpState) { w.timeDialog(d, st) })
}

func (w *window) timeDialog(d *axis.Device, st ntpState) {
	cfg := st.Cfg
	tz := entryRow("Time zone (IANA, e.g. Europe/Vienna)", localTZ())

	ntpOn := switchRow("Synchronize time via NTP", cfg.Enabled)
	ntpDHCP := switchRow("Obtain NTP servers via DHCP", cfg.FromDHCP)
	srv := []string{"", ""}
	copy(srv, cfg.Servers)
	if !cfg.Enabled && srv[0] == "" {
		srv[0] = "pool.ntp.org"
	}
	ntp1, ntp2 := entryRow("NTP server 1", srv[0]), entryRow("NTP server 2", srv[1])
	manual := switchRow("Take the time from this computer now", !cfg.Enabled)
	manual.SetSubtitle("Only useful without NTP")

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

	dlg := newFormDialog("Date & time – "+label(d), "Apply", 500)
	dlg.add(heading("Time zone"))
	dlg.add(boxedList(tz))
	dlg.add(heading("Time synchronization (NTP)"))
	if st.Err != nil {
		ntpOn.SetSensitive(false)
		dlg.add(note("NTP settings could not be read: " + st.Err.Error()))
	} else {
		dlg.add(boxedList(ntpOn, ntpDHCP, ntp1, ntp2))
		var info []string
		if cfg.Enabled && len(cfg.Active) > 0 {
			info = append(info, "Servers in use: "+strings.Join(cfg.Active, ", "))
		}
		if cfg.Enabled && cfg.Synced != nil {
			info = append(info, map[bool]string{true: "Time is synchronized.",
				false: "Time is not synchronized yet."}[*cfg.Synced])
		}
		if cfg.Legacy {
			info = append(info, "Older firmware: only one NTP server via param.cgi.")
		}
		if len(info) > 0 {
			dlg.add(note(strings.Join(info, "\n")))
		}
	}
	dlg.add(heading("Manual"))
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
			w.toast("Date & time saved")
		}, func(err error) {
			dlg.busy(false)
			dlg.toast(err.Error())
		})
	})
	dlg.Present(w)
}

// ---------------------------------------------------------------- Preview

const previewWidth = 1280

func (w *window) onPreview() {
	d := w.selected()
	if d == nil {
		return
	}
	if d.NeedsSetup != nil && *d.NeedsSetup {
		w.toast("Set a password first")
		w.onPassword()
		return
	}
	withLogin(w, d, "Preview", func(ctx context.Context, c *axis.Client) ([]byte, error) {
		return c.Snapshot(ctx, previewWidth)
	}, func(img []byte) { w.previewDialog(d, img) })
}

func (w *window) previewDialog(d *axis.Device, first []byte) {
	dlg := adw.NewDialog()
	dlg.SetTitle("Preview – " + label(d))
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
	live.SetTooltipText("Refresh automatically (1 image/s)")
	live.SetActive(true)
	refresh := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refresh.SetTooltipText("Reload image")

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
			status.SetText("Image not readable: " + err.Error())
			return
		}
		pic.SetPaintable(tex)
		status.SetText(fmt.Sprintf("%d × %d · as of %s", tex.Width(), tex.Height(), time.Now().Format("15:04:05")))
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
	dlg := adw.NewAlertDialog("Restart device?", fmt.Sprintf("%s (%s) will be restarted.", label(d), d.IP))
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("restart", "Restart")
	dlg.SetResponseAppearance("restart", adw.ResponseDestructive)
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(r string) {
		if r != "restart" {
			return
		}
		withLogin(w, d, "Restart", func(ctx context.Context, c *axis.Client) (struct{}, error) {
			return struct{}{}, c.Restart(ctx)
		}, func(struct{}) { w.toast("Restart triggered") })
	})
	dlg.Present(w)
}

// onReset resets the device to factory defaults after confirmation.
func (w *window) onReset() {
	d := w.selected()
	if d == nil {
		return
	}
	dlg := adw.NewAlertDialog("Reset to factory defaults?",
		fmt.Sprintf("%s (%s)\n\nAll settings, users and passwords will be deleted. "+
			"The device restarts and must then be set up again with an initial password.",
			label(d), d.IP))
	keep := gtk.NewCheckButtonWithLabel("Keep network settings (IP address)")
	keep.SetActive(true)
	hint := note("")
	hint.SetMarginStart(28)
	updHint := func() {
		if keep.Active() {
			hint.SetText("The device stays reachable at " + d.IP + ".")
		} else {
			hint.SetText("The device then obtains an address via DHCP; without a DHCP server " +
				"it uses 192.168.0.90.")
		}
	}
	keep.ConnectToggled(updHint)
	updHint()
	extra := gtk.NewBox(gtk.OrientationVertical, 6)
	extra.Append(keep)
	extra.Append(hint)
	dlg.SetExtraChild(extra)
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("reset", "Reset")
	dlg.SetResponseAppearance("reset", adw.ResponseDestructive)
	dlg.SetDefaultResponse("cancel")
	dlg.SetCloseResponse("cancel")
	key := d.Key()
	dlg.ConnectResponse(func(r string) {
		if r != "reset" {
			return
		}
		keepNet := keep.Active()
		withLogin(w, d, "Reset", func(ctx context.Context, c *axis.Client) (struct{}, error) {
			if err := c.CheckLogin(ctx); err != nil { // don't allow triggering it anonymously
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
				w.toast(label(d) + " is being reset and restarts (about 1–2 minutes)")
			} else {
				w.toast(label(d) + " is being reset – search again afterwards, the IP address changes")
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
