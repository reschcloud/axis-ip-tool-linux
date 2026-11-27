// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// The action bar buttons are flat by default in libadwaita. Give them a card-like
// background so they are recognizable as buttons; the login lock sits inside the card.
const appCSS = `
button.action-button {
	border-radius: 8px;
	padding: 4px 10px;
	min-height: 30px;
}
button.action-button:not(.destructive-action) {
	background-color: rgba(128, 128, 128, 0.16);
	background-image: none;
	border: 1px solid rgba(128, 128, 128, 0.38);
}
button.action-button:not(.destructive-action):hover {
	background-color: rgba(128, 128, 128, 0.26);
	border-color: rgba(128, 128, 128, 0.58);
}
button.action-button:not(.destructive-action):active {
	background-color: rgba(128, 128, 128, 0.36);
}
button.action-button.destructive-action {
	background-color: #c01c28;
	background-image: none;
	color: #ffffff;
	border: 1px solid #a51d2d;
}
button.action-button.destructive-action:hover {
	background-color: #a51d2d;
}
button.action-button.destructive-action:active {
	background-color: #8d151f;
}
`

// installStyles registers the application CSS once a display is available.
func installStyles() {
	p := gtk.NewCSSProvider()
	p.LoadFromString(appCSS)
	if d := gdk.DisplayGetDefault(); d != nil {
		gtk.StyleContextAddProviderForDisplay(d, p, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
	}
}
