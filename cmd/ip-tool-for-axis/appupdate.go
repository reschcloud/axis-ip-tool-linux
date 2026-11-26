// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"ip-tool-for-axis/internal/appupdate"
)

func (w *window) appUpdateButton() *gtk.Button {
	button := gtk.NewButtonWithLabel("Update available")
	button.AddCSSClass("flat")
	button.SetVisible(false)

	ctx, cancel := context.WithCancel(context.Background())
	w.ConnectCloseRequest(func() bool {
		cancel()
		return false
	})
	go func() {
		tag, err := appupdate.Check(ctx, version)
		if err != nil || tag == "" {
			return // Offline or up to date: leave the indicator hidden.
		}
		ui(func() {
			if ctx.Err() != nil {
				return
			}
			button.SetTooltipText("View " + appName + " release " + tag + " on GitHub")
			button.ConnectClicked(func() {
				gtk.NewURILauncher(appupdate.ReleaseURL(tag)).Launch(ctx, &w.Window, nil)
			})
			button.SetVisible(true)
		})
	}()
	return button
}
