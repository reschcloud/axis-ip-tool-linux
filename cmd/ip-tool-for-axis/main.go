// SPDX-License-Identifier: AGPL-3.0-or-later

// ip-tool-for-axis – GNOME app for finding Axis devices and doing their basic configuration,
// modeled on the AXIS IP Utility.
package main

import (
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"ip-tool-for-axis/internal/axis"
)

const (
	appID   = "io.github.reschcloud.IPToolForAXIS"
	appName = "IP Tool for AXIS (Linux)"
)

// version is set at build time (-ldflags "-X main.version=…", see Makefile).
var version = "dev"

func main() {
	axis.UserAgent = "ip-tool-for-axis/" + version
	if s, err := loadSettings(); err == nil {
		axis.SetHTTPSOnly(s.HTTPSOnly)
	}
	flags := gio.ApplicationFlagsNone
	shot := os.Getenv("IP_TOOL_FOR_AXIS_SCREENSHOT")
	if shot != "" {
		flags = gio.ApplicationNonUnique // separate instance even if the app is already running
	}
	glib.SetApplicationName(appName)
	app := adw.NewApplication(appID, flags)

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
		w := newWindow(app)
		w.Present()
		if shot != "" {
			w.SetDefaultSize(1400, 300)
			w.screenshotAndQuit(shot, 12) // see screenshot.go
		}
	})
	os.Exit(app.Run(os.Args))
}

func showAbout(app *adw.Application) {
	d := adw.NewAboutDialog()
	d.SetApplicationName(appName)
	d.SetApplicationIcon(appID)
	d.SetVersion(version)
	d.SetComments("Find Axis devices on the network and do their basic configuration – " +
		"inspired by the AXIS IP Utility.\n\nNot an official product of Axis Communications AB.")
	d.SetLicenseType(gtk.LicenseAGPL30)
	d.Present(app.ActiveWindow())
}
