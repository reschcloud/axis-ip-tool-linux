// SPDX-License-Identifier: GPL-3.0-or-later

// axis-ip-tool – GNOME-App zum Finden und Grundkonfigurieren von Axis-Geräten,
// angelehnt an die AXIS IP Utility.
package main

import (
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"axis-ip-tool/internal/axis"
)

const appID = "io.github.reschcloud.AxisIpTool"

// version wird beim Bauen gesetzt (-ldflags "-X main.version=…", siehe Makefile).
var version = "dev"

func main() {
	if s, err := loadSettings(); err == nil {
		axis.SetHTTPSOnly(s.HTTPSOnly)
	}
	app := adw.NewApplication(appID, gio.ApplicationFlagsNone)

	about := gio.NewSimpleAction("about", nil)
	about.ConnectActivate(func(_ *glibVariant) { showAbout(app) })
	app.AddAction(about)

	quit := gio.NewSimpleAction("quit", nil)
	quit.ConnectActivate(func(_ *glibVariant) { app.Quit() })
	app.AddAction(quit)
	app.SetAccelsForAction("app.quit", []string{"<Control>q"})
	app.SetAccelsForAction("win.rescan", []string{"F5", "<Control>r"})
	app.SetAccelsForAction("win.search", []string{"<Control>f"})

	app.ConnectActivate(func() {
		if w := app.ActiveWindow(); w != nil {
			w.Present()
			return
		}
		newWindow(app).Present()
	})
	os.Exit(app.Run(os.Args))
}

func showAbout(app *adw.Application) {
	d := adw.NewAboutDialog()
	d.SetApplicationName("Axis IP Tool")
	d.SetApplicationIcon(appID)
	d.SetVersion(version)
	d.SetComments("Axis-Geräte im Netzwerk finden und grundlegend konfigurieren – " +
		"inspiriert von der AXIS IP Utility.\n\nKein offizielles Produkt der Axis Communications AB.")
	d.SetLicenseType(gtk.LicenseGPL30)
	d.Present(app.ActiveWindow())
}
