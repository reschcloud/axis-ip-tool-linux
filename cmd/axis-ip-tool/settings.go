// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settings are the persistently stored settings of the app.
type settings struct {
	HTTPSOnly bool `json:"httpsOnly"`
	// commissioning; nil = default (50 Hz, ceiling), 0 or "" = don't change
	PowerFrequency *int    `json:"powerFrequency,omitempty"`
	Mount          *string `json:"mount,omitempty"`
}

// updateSettings reads the settings, changes them with fn and saves them again.
func updateSettings(fn func(*settings)) error {
	s, _ := loadSettings() // missing/broken file → defaults
	fn(&s)
	return saveSettings(s)
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "axis-ip-tool", "settings.json"), nil
}

func loadSettings() (settings, error) {
	var s settings
	p, err := settingsPath()
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveSettings(s settings) error {
	p, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o644)
}
