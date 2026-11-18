// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// InitialSetup are image settings for commissioning – the same ones the setup wizard of
// the web interface asks for. Zero values mean "don't change".
type InitialSetup struct {
	PowerFrequency int    // power line frequency in Hz: 50 or 60
	Mount          string // mounting position: MountCeiling, MountWall, MountDesk
}

// Mounting positions (values of ImageSource.I<n>.CameraTiltOrientation).
const (
	MountCeiling = "ceiling"
	MountWall    = "wall"
	MountDesk    = "desk"
)

var tiltValue = map[string]string{MountCeiling: "-90", MountWall: "0", MountDesk: "90"}

// Names of the settings in ApplyInitialSetup's skipped list.
const (
	SettingPowerFrequency = "Power line frequency"
	SettingMount          = "Mounting position"
)

// MountName returns the display name of a mounting position.
func MountName(m string) string {
	switch m {
	case MountCeiling:
		return "Ceiling"
	case MountWall:
		return "Wall"
	case MountDesk:
		return "Desk"
	}
	return ""
}

// ParseMount accepts the names "ceiling", "wall", "desk" and "none".
func ParseMount(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "-", "none":
		return "", nil
	case "ceiling":
		return MountCeiling, nil
	case "wall":
		return MountWall, nil
	case "desk":
		return MountDesk, nil
	}
	return "", fmt.Errorf("unknown mounting position %q (ceiling, wall, desk, none)", s)
}

// Empty: nothing to do.
func (s InitialSetup) Empty() bool { return s.PowerFrequency == 0 && s.Mount == "" }

func (s InitialSetup) validate() error {
	if s.PowerFrequency != 0 && s.PowerFrequency != 50 && s.PowerFrequency != 60 {
		return fmt.Errorf("invalid power line frequency %d Hz (50 or 60)", s.PowerFrequency)
	}
	if _, ok := tiltValue[s.Mount]; s.Mount != "" && !ok {
		return fmt.Errorf("unknown mounting position %q", s.Mount)
	}
	return nil
}

var reImageSourceParam = regexp.MustCompile(`^ImageSource\.I\d+\.(CaptureFrequency|CameraTiltOrientation)$`)

// ApplyInitialSetup sets power line frequency and mounting position on all image sources
// that have these settings (thermal cameras, for example, have no power line frequency).
// skipped lists the settings the device doesn't know.
func (c *Client) ApplyInitialSetup(ctx context.Context, s InitialSetup) (skipped []string, err error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.Empty() {
		return nil, nil
	}
	p, err := c.params(ctx, "ImageSource")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	var freq, tilt bool
	for k := range p {
		m := reImageSourceParam.FindStringSubmatch(k)
		switch {
		case m == nil:
		case m[1] == "CaptureFrequency" && s.PowerFrequency != 0:
			q.Set(k, fmt.Sprintf("%dHz", s.PowerFrequency))
			freq = true
		case m[1] == "CameraTiltOrientation" && s.Mount != "":
			q.Set(k, tiltValue[s.Mount])
			tilt = true
		}
	}
	if s.PowerFrequency != 0 && !freq {
		skipped = append(skipped, SettingPowerFrequency)
	}
	if s.Mount != "" && !tilt {
		skipped = append(skipped, SettingMount)
	}
	if len(q) == 0 {
		return skipped, nil
	}
	if freq {
		// Like the web interface wizard: the power line frequency question is done.
		q.Set("System.CaptureFrequencySet", "yes")
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys) // fixed order (tests, traceability)
	var b strings.Builder
	b.WriteString("action=update")
	for _, k := range keys {
		b.WriteString("&" + url.QueryEscape(k) + "=" + url.QueryEscape(q.Get(k)))
	}
	_, data, err := c.do(ctx, http.MethodPost, "/axis-cgi/param.cgi", []byte(b.String()),
		"application/x-www-form-urlencoded", true)
	if err != nil {
		return skipped, err
	}
	if strings.Contains(strings.ToLower(string(data)), "error") {
		return skipped, errors.New(strings.TrimSpace(string(data)))
	}
	return skipped, nil
}

// String describes the settings, e.g. "power line frequency 50 Hz, mounting position Ceiling".
func (s InitialSetup) String() string {
	var parts []string
	if s.PowerFrequency != 0 {
		parts = append(parts, fmt.Sprintf("power line frequency %d Hz", s.PowerFrequency))
	}
	if s.Mount != "" {
		parts = append(parts, "mounting position "+MountName(s.Mount))
	}
	return strings.Join(parts, ", ")
}
