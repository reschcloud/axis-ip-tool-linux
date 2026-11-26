// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"slices"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"

	"ip-tool-for-axis/internal/axis"
)

// Commissioning: power line frequency and mounting position (like the setup wizard of
// the web interface). The last choice is remembered in the settings.

var (
	freqValues  = []int{50, 60, 0}
	mountValues = []string{axis.MountCeiling, axis.MountWall, axis.MountDesk, ""}
)

// setupRows returns the selection rows, preset with the last choice.
func setupRows() (freq, mount *adw.ComboRow) {
	cur := savedSetup()
	freq = comboRow("Power line frequency", "50 Hz", "60 Hz", "don't change")
	freq.SetSubtitle("Prevents flicker under artificial light")
	freq.SetSelected(uint(max(0, slices.Index(freqValues, cur.PowerFrequency))))
	mount = comboRow("Mounting position", "Ceiling", "Wall", "Desk", "don't change")
	mount.SetSelected(uint(max(0, slices.Index(mountValues, cur.Mount))))
	return freq, mount
}

// readSetup reads the selection and remembers it for next time.
func readSetup(freq, mount *adw.ComboRow) axis.InitialSetup {
	s := axis.InitialSetup{PowerFrequency: freqValues[freq.Selected()], Mount: mountValues[mount.Selected()]}
	updateSettings(func(st *settings) { st.PowerFrequency, st.Mount = &s.PowerFrequency, &s.Mount })
	return s
}

func savedSetup() axis.InitialSetup {
	s := axis.InitialSetup{PowerFrequency: 50, Mount: axis.MountCeiling}
	if st, err := loadSettings(); err == nil {
		if st.PowerFrequency != nil && slices.Contains(freqValues, *st.PowerFrequency) {
			s.PowerFrequency = *st.PowerFrequency
		}
		if st.Mount != nil && slices.Contains(mountValues, *st.Mount) {
			s.Mount = *st.Mount
		}
	}
	return s
}

// setupResult describes the result for a message (", 50 Hz, Ceiling").
func setupResult(s axis.InitialSetup, skipped []string, err error) string {
	if err != nil {
		return fmt.Sprintf(" – power line frequency/mounting position not set: %v", err)
	}
	out := ""
	if s.PowerFrequency != 0 && !slices.Contains(skipped, axis.SettingPowerFrequency) {
		out += fmt.Sprintf(", %d Hz", s.PowerFrequency)
	}
	if s.Mount != "" && !slices.Contains(skipped, axis.SettingMount) {
		out += ", " + axis.MountName(s.Mount)
	}
	return out
}
