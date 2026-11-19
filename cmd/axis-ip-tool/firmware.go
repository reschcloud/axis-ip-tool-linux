// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"axis-ip-tool/internal/axis"
)

// Firmware update: compare with the AXIS OS versions on axis.com, download (My Axis
// sign-in), upload via firmwaremanagement.cgi, wait for the restart.

// fwCatalog is shared by the list check and the dialog (caches the listings).
var fwCatalog = axis.NewFirmwareCatalog()

// ---------------------------------------------------------------- List: update hints

// fwHints: model|firmware → newer version on the device's track ("" = none/unknown).
// GTK thread only.
var fwHints = map[string]string{}

func fwHintKey(d *axis.Device) string { return d.Model + "|" + d.Firmware }

// firmwareText is the text of the firmware column.
func firmwareText(d *axis.Device) string {
	if v := fwHints[fwHintKey(d)]; v != "" {
		return d.Firmware + "  →  " + v
	}
	return d.Firmware
}

// checkFirmware looks up (once per model and firmware) whether axis.com has a newer
// version on the device's track and shows it in the firmware column.
func (w *window) checkFirmware(key string) {
	d := w.reg.Get(key)
	if d == nil || d.Model == "" || d.Firmware == "" || !w.fwCheck {
		return
	}
	hk := fwHintKey(d)
	if _, done := fwHints[hk]; done {
		return
	}
	fwHints[hk] = "" // running/checked
	model, fw := d.Model, d.Firmware
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		newer := ""
		cur, ok := axis.ParseFWVersion(fw)
		p, found, err := fwCatalog.FindProduct(ctx, model)
		if ok && found && err == nil {
			if vs, err := fwCatalog.Versions(ctx, p); err == nil {
				for _, c := range axis.FirmwareChoices(cur, vs) {
					if strings.HasPrefix(c.Label, "Current track") {
						newer = c.Version.String()
					}
				}
			}
		}
		glib.IdleAdd(func() {
			if err != nil {
				delete(fwHints, hk) // try again later (offline?)
				return
			}
			fwHints[hk] = newer
			if newer == "" {
				return
			}
			for _, dev := range w.reg.All() {
				if fwHintKey(dev) == hk {
					w.rerender(dev.Key())
				}
			}
		})
	}()
}

// ---------------------------------------------------------------- Dialog

type fwInfo struct {
	model, prodNbr, firmware string
	product                  axis.FirmwareProduct
	found                    bool
	choices                  []axis.FirmwareChoice
	catalogErr               error
}

func loadFirmwareInfo(ctx context.Context, c *axis.Client) (fwInfo, error) {
	if err := c.CheckLogin(ctx); err != nil {
		return fwInfo{}, err
	}
	info, err := c.BasicInfo(ctx)
	if err != nil {
		return fwInfo{}, err
	}
	fi := fwInfo{model: info.Model, firmware: info.Firmware}
	fi.prodNbr, _ = c.ProductNumber(ctx)
	fi.product, fi.found, fi.catalogErr = fwCatalog.FindProduct(ctx, fi.prodNbr, fi.model)
	if fi.catalogErr == nil && fi.found {
		var vs []axis.FWVersion
		if vs, fi.catalogErr = fwCatalog.Versions(ctx, fi.product); fi.catalogErr == nil {
			cur, _ := axis.ParseFWVersion(fi.firmware)
			fi.choices = axis.FirmwareChoices(cur, vs)
		}
	}
	return fi, nil
}

type fwRow struct {
	d       *axis.Device
	key     string
	row     *adw.ActionRow
	dd      *gtk.DropDown
	items   *gtk.StringList
	login   *gtk.Button
	bar     *gtk.ProgressBar
	info    *fwInfo
	hasFile bool // last item is the local file
	running bool
}

// target: 0 = don't update, 1…n = choice, n+1 = local file
func (r *fwRow) target() int {
	if r.info == nil || r.dd == nil {
		return 0
	}
	return int(r.dd.Selected())
}

type fwDialog struct {
	w       *window
	dlg     *formDialog
	rows    []*fwRow
	account *adw.ActionRow
	accBtn  *gtk.Button
	factory *adw.ComboRow
	file    string
	fileRow *adw.ActionRow
	session axis.MyAxisSession
	signed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	running int
}

func (w *window) onFirmware() {
	var devs []*axis.Device
	for _, d := range w.selectedDevices() {
		if d.Online && d.IP != "" {
			devs = append(devs, d)
		}
	}
	if len(devs) == 0 {
		return
	}
	f := &fwDialog{w: w}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.session, f.signed = axis.LoadMyAxisSession()
	f.dlg = newFormDialog("Firmware update", "Update", 760)
	f.dlg.SetContentHeight(620)
	f.dlg.apply.SetSensitive(false)
	f.dlg.apply.ConnectClicked(f.start)
	f.dlg.ConnectClosed(func() { f.cancel() })

	// account
	f.account = adw.NewActionRow()
	f.account.SetTitle("Axis account (My Axis)")
	f.accBtn = gtk.NewButton()
	f.accBtn.SetVAlign(gtk.AlignCenter)
	f.accBtn.ConnectClicked(f.onAccount)
	f.account.AddSuffix(f.accBtn)
	f.updateAccount()
	f.dlg.add(heading("Download"))
	f.fileRow = adw.NewActionRow()
	f.fileRow.SetTitle("Local firmware file")
	f.fileRow.SetSubtitle("Optional – instead of a download from axis.com")
	choose := gtk.NewButtonWithLabel("Choose …")
	choose.SetVAlign(gtk.AlignCenter)
	choose.ConnectClicked(f.chooseFile)
	f.fileRow.AddSuffix(choose)
	f.dlg.add(boxedList(f.account, f.fileRow))

	// devices
	f.dlg.add(heading("Devices"))
	list := boxedList()
	for _, d := range devs {
		r := &fwRow{d: d, key: d.Key()}
		r.row = adw.NewActionRow()
		r.row.SetTitle(label(d) + " – " + d.Model)
		r.row.SetSubtitle("Installed " + dashIfEmpty(d.Firmware) + " · checking …")
		r.bar = gtk.NewProgressBar()
		r.bar.SetVAlign(gtk.AlignCenter)
		r.bar.SetSizeRequest(160, -1)
		r.bar.SetVisible(false)
		r.row.AddSuffix(r.bar)
		r.login = gtk.NewButtonWithLabel("Log in …")
		r.login.SetVAlign(gtk.AlignCenter)
		r.login.SetVisible(false)
		r.login.ConnectClicked(func() { f.loginRow(r) })
		r.row.AddSuffix(r.login)
		list.Append(r.row)
		f.rows = append(f.rows, r)
	}
	f.dlg.add(list)

	f.dlg.add(heading("Options"))
	f.factory = comboRow("Settings after the update", "Keep all settings",
		"Factory defaults, keep network settings", "Complete factory defaults")
	f.dlg.add(boxedList(f.factory))
	f.dlg.add(note("The device verifies the firmware signature before installing it. " +
		"Installing and restarting takes several minutes; keep the device powered. " +
		"Downloads are verified with the SHA-256 checksum from axis.com and kept in " +
		"~/.cache/axis-ip-tool/firmware."))

	for _, r := range f.rows {
		if _, ok := w.creds[r.key]; ok {
			f.loadRow(r)
		} else {
			f.needLogin(r, "Not logged in")
		}
	}
	f.dlg.Present(w)
}

func (f *fwDialog) toast(msg string) { f.dlg.toast(msg) }

func (f *fwDialog) updateAccount() {
	switch {
	case f.signed:
		sub := "Signed in"
		if !f.session.Expires.IsZero() {
			sub += " – session valid until " + f.session.Expires.Local().Format("2006-01-02")
		}
		f.account.SetSubtitle(sub)
		f.accBtn.SetLabel("Sign out")
		f.accBtn.SetSensitive(true)
	case webkitAvailable():
		f.account.SetSubtitle("Not signed in – needed to download firmware from axis.com")
		f.accBtn.SetLabel("Sign in …")
		f.accBtn.SetSensitive(true)
	default:
		f.account.SetSubtitle("Not signed in. " + webkitHint)
		f.accBtn.SetLabel("Sign in …")
		f.accBtn.SetSensitive(false)
	}
}

func (f *fwDialog) onAccount() {
	if f.signed {
		if err := myAxisSignOut(); err != nil {
			f.toast(err.Error())
		}
		f.signed, f.session = false, axis.MyAxisSession{}
		f.updateAccount()
		return
	}
	f.signIn(nil)
}

// signIn opens the sign-in; then (optional) continues.
func (f *fwDialog) signIn(then func()) {
	ok := myAxisLogin(f.dlg, fwCatalog, func(s axis.MyAxisSession) {
		f.session, f.signed = s, true
		f.updateAccount()
		f.toast("Signed in to My Axis")
		if then != nil {
			then()
		}
	})
	if !ok {
		f.toast(webkitHint)
	}
}

func (f *fwDialog) chooseFile() {
	fd := gtk.NewFileDialog()
	fd.SetTitle("Choose firmware file")
	filter := gtk.NewFileFilter()
	filter.SetName("AXIS OS firmware (*.bin)")
	filter.AddPattern("*.bin")
	filters := gio.NewListStore(gtk.GTypeFileFilter)
	filters.Append(filter.Object)
	fd.SetFilters(filters)
	fd.Open(context.Background(), &f.w.Window, func(res gio.AsyncResulter) {
		file, err := fd.OpenFinish(res)
		if err != nil || file == nil {
			return
		}
		f.file = file.Path()
		f.fileRow.SetSubtitle(f.file)
		for _, r := range f.rows {
			if r.info == nil {
				continue
			}
			f.fillChoices(r)
			if len(f.rows) == 1 {
				r.dd.SetSelected(r.items.NItems() - 1)
			}
		}
		f.updateApply()
	})
}

func (f *fwDialog) needLogin(r *fwRow, msg string) {
	r.row.SetSubtitle("Installed " + dashIfEmpty(r.d.Firmware) + " · " + msg)
	r.login.SetVisible(true)
}

func (f *fwDialog) loginRow(r *fwRow) {
	withLogin(f.w, r.d, "Firmware", loadFirmwareInfo, func(fi fwInfo) {
		if f.ctx.Err() == nil {
			f.showInfo(r, fi)
		}
	})
}

func (f *fwDialog) loadRow(r *fwRow) {
	c := f.w.creds[r.key]
	ip, port := r.d.IP, r.d.Port
	async(func() (fwInfo, error) {
		ctx, cancel := context.WithTimeout(f.ctx, 40*time.Second)
		defer cancel()
		return loadFirmwareInfo(ctx, axis.NewClient(ip, port, c[0], c[1]))
	}, func(fi fwInfo) {
		if f.ctx.Err() == nil {
			f.showInfo(r, fi)
		}
	}, func(err error) {
		if f.ctx.Err() != nil {
			return
		}
		if errors.Is(err, axis.ErrAuth) {
			f.w.setAuth(r.key, false)
			f.needLogin(r, err.Error())
			return
		}
		r.row.SetSubtitle("Installed " + dashIfEmpty(r.d.Firmware) + " · " + err.Error())
	})
}

func (f *fwDialog) showInfo(r *fwRow, fi fwInfo) {
	r.info = &fi
	r.login.SetVisible(false)
	sub := "Installed " + dashIfEmpty(fi.firmware)
	switch {
	case fi.catalogErr != nil:
		sub += " · axis.com: " + fi.catalogErr.Error()
	case !fi.found:
		sub += " · no firmware for this model on axis.com"
	case len(fi.choices) == 0:
		sub += " · up to date"
	default:
		cur, _ := axis.ParseFWVersion(fi.firmware)
		if cur != nil {
			sub += " (" + cur.TrackName() + ")"
		}
		sub += " · newer: " + fi.choices[0].Version.String()
	}
	r.row.SetSubtitle(sub)
	f.fillChoices(r)
	if len(fi.choices) > 0 {
		r.dd.SetSelected(1) // the first suggestion (current track if newer)
	}
	f.updateApply()
}

func (f *fwDialog) fillChoices(r *fwRow) {
	items := []string{"Don't update"}
	for _, c := range r.info.choices {
		items = append(items, fmt.Sprintf("%s – %s", c.Version, c.Label))
	}
	r.hasFile = f.file != ""
	if r.hasFile {
		items = append(items, "File: "+filepath.Base(f.file))
	}
	sel := uint(0)
	if r.dd != nil {
		sel = r.dd.Selected()
		r.row.Remove(r.dd)
	}
	r.items = gtk.NewStringList(items)
	r.dd = gtk.NewDropDown(r.items, nil)
	r.dd.SetVAlign(gtk.AlignCenter)
	if sel < r.items.NItems() {
		r.dd.SetSelected(sel)
	}
	r.dd.NotifyProperty("selected", f.updateApply)
	r.row.AddSuffix(r.dd)
}

func (f *fwDialog) updateApply() {
	n := 0
	for _, r := range f.rows {
		if r.target() > 0 && !r.running {
			n++
		}
	}
	f.dlg.apply.SetSensitive(n > 0 && f.running == 0)
	switch n {
	case 0, 1:
		f.dlg.apply.SetLabel("Update")
	default:
		f.dlg.apply.SetLabel(fmt.Sprintf("Update %d devices", n))
	}
}

func (f *fwDialog) factoryMode() string {
	return []string{axis.FactoryDefaultNone, axis.FactoryDefaultSoft, axis.FactoryDefaultHard}[f.factory.Selected()]
}

func (f *fwDialog) start() {
	var todo []*fwRow
	needDownload := false
	for _, r := range f.rows {
		if t := r.target(); t > 0 && !r.running {
			todo = append(todo, r)
			if !(r.hasFile && t == int(r.items.NItems())-1) {
				needDownload = true
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	if needDownload && !f.signed {
		if !webkitAvailable() {
			f.toast("Downloads need a My Axis sign-in. " + webkitHint)
			return
		}
		f.signIn(f.start)
		return
	}
	body := fmt.Sprintf("%d device(s) will install the new firmware and restart. "+
		"They are unavailable for several minutes.", len(todo))
	if f.factoryMode() != axis.FactoryDefaultNone {
		body += "\n\nAll settings are reset to factory defaults"
		if f.factoryMode() == axis.FactoryDefaultSoft {
			body += " (network settings are kept)"
		}
		body += " – the device has no password afterwards."
	}
	alert := adw.NewAlertDialog("Update firmware?", body)
	alert.AddResponse("cancel", "Cancel")
	alert.AddResponse("update", "Update")
	alert.SetResponseAppearance("update", adw.ResponseDestructive)
	alert.SetCloseResponse("cancel")
	alert.ConnectResponse(func(resp string) {
		if resp != "update" {
			return
		}
		for _, r := range todo {
			f.run(r)
		}
		f.updateApply()
	})
	alert.Present(f.dlg)
}

// downloads of the same file run only once (the second one finds it in the cache)
var dlLocks sync.Map

func (f *fwDialog) run(r *fwRow) {
	t := r.target()
	local := r.hasFile && t == int(r.items.NItems())-1
	var choice axis.FirmwareChoice
	if !local {
		choice = r.info.choices[t-1]
	}
	file, session, factory := f.file, f.session.Value, f.factoryMode()
	creds := f.w.creds[r.key]
	ip, port, old, product := r.d.IP, r.d.Port, r.info.firmware, r.info.product
	r.running = true
	f.running++
	r.dd.SetSensitive(false)
	r.bar.SetVisible(true)
	r.bar.SetFraction(0)

	set := func(text string, frac float64) {
		glib.IdleAdd(func() {
			if f.ctx.Err() != nil {
				return
			}
			r.row.SetSubtitle(text)
			if frac >= 0 {
				r.bar.SetFraction(frac)
			} else {
				r.bar.Pulse()
			}
		})
	}
	progress := func(what string) func(done, total int64) {
		return func(done, total int64) {
			if total > 0 {
				p := float64(done) / float64(total)
				set(fmt.Sprintf("%s … %d %%", what, int(p*100)), p)
			}
		}
	}
	go func() {
		err := func() error {
			path := file
			if !local {
				ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
				ff, err := fwCatalog.File(ctx, product, choice.Version)
				cancel()
				if err != nil {
					return err
				}
				mu, _ := dlLocks.LoadOrStore(ff.SHA256, &sync.Mutex{})
				mu.(*sync.Mutex).Lock()
				set("Downloading "+ff.Name+" …", 0)
				path, err = fwCatalog.Download(f.ctx, ff, session, progress("Downloading "+ff.Version.String()))
				mu.(*sync.Mutex).Unlock()
				if err != nil {
					return err
				}
			}
			c := axis.NewClient(ip, port, creds[0], creds[1])
			set("Uploading to the device …", 0)
			newVer, err := c.UpgradeFirmware(f.ctx, path, factory, progress("Uploading"))
			if err != nil {
				return err
			}
			if factory != axis.FactoryDefaultNone {
				set("Installing "+newVer+" and resetting to factory defaults – the device restarts", 1)
				return nil
			}
			// pulse while waiting
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(300 * time.Millisecond):
						glib.IdleAdd(func() { r.bar.Pulse() })
					}
				}
			}()
			defer close(stop)
			err = c.WaitForUpgrade(f.ctx, old, newVer, func(s axis.UpgradeStage) {
				switch s {
				case axis.StageInstalling:
					set("Installing "+newVer+" …", -1)
				case axis.StageRestarting:
					set("Restarting …", -1)
				}
			})
			if err != nil {
				return err
			}
			set("Updated to "+newVer, 1)
			return nil
		}()
		glib.IdleAdd(func() {
			if f.ctx.Err() != nil {
				return
			}
			r.running = false
			f.running--
			r.dd.SetSensitive(true)
			if err != nil {
				r.bar.SetVisible(false)
				r.row.SetSubtitle("Failed: " + err.Error())
				if errors.Is(err, axis.ErrMyAxisLogin) {
					axis.ForgetMyAxisSession()
					f.signed = false
					f.updateAccount()
				}
				if errors.Is(err, axis.ErrAuth) {
					f.w.setAuth(r.key, false)
				}
			} else {
				r.bar.SetFraction(1)
				r.dd.SetSelected(0)
				r.dd.SetSensitive(false)
			}
			delete(f.w.infoAt, r.key) // read the new firmware into the list
			f.w.refreshInfo(r.key)
			f.updateApply()
		})
	}()
}
