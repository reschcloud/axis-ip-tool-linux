// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// settings are the persistently stored settings of the app.
type settings struct {
	HTTPSOnly bool `json:"httpsOnly"`
	// don't compare the firmware with axis.com in the background
	NoFirmwareCheck bool `json:"noFirmwareCheck,omitempty"`
	// commissioning; nil = default (50 Hz, ceiling), 0 or "" = don't change
	PowerFrequency *int    `json:"powerFrequency,omitempty"`
	Mount          *string `json:"mount,omitempty"`
	// manually added cameras (IPv4 or IPv6), monitored like discovered devices
	Manual []string `json:"manualCameras,omitempty"`
}

// updateSettings reads the settings, changes them with fn and saves them again.
func updateSettings(fn func(*settings)) error {
	s, err := loadSettings()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err // don't overwrite a broken settings file
	}
	fn(&s)
	return saveSettings(s)
}

func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ip-tool-for-axis", "settings.json"), nil
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
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically: a crash must not leave a truncated settings.json.
	f, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// firmwareCheckEnabled: compare the firmware with axis.com in the background (default on).
func firmwareCheckEnabled() bool {
	s, _ := loadSettings()
	return !s.NoFirmwareCheck
}
