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
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- PTZ (ptz.cgi)

// PTZCaps lists what ptz.cgi offers for video channel 1 (PTZ.Support.S1.*). Fixed cameras
// with a motorized lens may report only zoom/focus here.
type PTZCaps struct {
	PanTilt   bool // continuous pan and/or tilt
	Zoom      bool // continuous zoom
	RelZoom   bool // relative zoom (rzoom)
	AbsZoom   bool // absolute zoom (zoom=1…9999)
	Focus     bool // continuous focus
	RelFocus  bool // relative focus (rfocus)
	AbsFocus  bool // absolute focus (focus=1…9999)
	AutoFocus bool
	AreaZoom  bool // center/zoom on an image position
	Presets   bool // server presets
}

// Any reports whether there is anything to control.
func (p PTZCaps) Any() bool {
	return p.PanTilt || p.AnyZoom() || p.AnyFocus() || p.AutoFocus || p.AreaZoom || p.Presets
}

// AnyZoom: zoom is controllable in some way.
func (p PTZCaps) AnyZoom() bool { return p.Zoom || p.RelZoom || p.AbsZoom }

// AnyFocus: focus is controllable in some way (autofocus not counted).
func (p PTZCaps) AnyFocus() bool { return p.Focus || p.RelFocus || p.AbsFocus }

// PTZPreset is a stored position.
type PTZPreset struct {
	No   int
	Name string
}

// PTZCapabilities reads the PTZ support of channel 1. Devices without PTZ return zero caps.
func (c *Client) PTZCapabilities(ctx context.Context) (PTZCaps, error) {
	p, err := c.params(ctx, "Properties.PTZ")
	if err != nil {
		return PTZCaps{}, err
	}
	if p["Properties.PTZ.PTZ"] != "yes" {
		return PTZCaps{}, nil
	}
	if p, err = c.params(ctx, "PTZ.Support"); err != nil {
		return PTZCaps{}, err
	}
	caps := parsePTZSupport(p)
	if caps.Any() {
		// Fixed cameras report digital PTZ (view areas) here, but channel 1 is the
		// overview where ptz.cgi refuses everything ("PTZ disabled", empty position).
		// Only offer PTZ if the channel reports a position.
		data, err := c.ptz(ctx, url.Values{"query": {"position"}})
		if errors.Is(err, ErrAuth) {
			return PTZCaps{}, err
		}
		if pos := parsePosition(string(data)); err != nil || !(pos.HasZoom || strings.Contains(string(data), "pan=")) {
			return PTZCaps{}, nil
		}
	}
	return caps, nil
}

func parsePTZSupport(p map[string]string) PTZCaps {
	s := func(k string) bool { return p["PTZ.Support.S1."+k] == "true" }
	return PTZCaps{
		PanTilt:   s("ContinuousPan") || s("ContinuousTilt"),
		Zoom:      s("ContinuousZoom"),
		RelZoom:   s("RelativeZoom"),
		AbsZoom:   s("AbsoluteZoom"),
		Focus:     s("ContinuousFocus"),
		RelFocus:  s("RelativeFocus"),
		AbsFocus:  s("AbsoluteFocus"),
		AutoFocus: s("AutoFocus"),
		AreaZoom:  s("AreaZoom"),
		Presets:   s("ServerPreset"),
	}
}

// ptz sends a ptz.cgi command for channel 1. Commas stay unescaped – some firmware
// doesn't decode %2C in values like "10,-20".
func (c *Client) ptz(ctx context.Context, q url.Values) ([]byte, error) {
	q.Set("camera", "1")
	path := "/axis-cgi/com/ptz.cgi?" + strings.ReplaceAll(q.Encode(), "%2C", ",")
	_, data, err := c.do(ctx, http.MethodGet, path, nil, "", true)
	if err != nil {
		return nil, err
	}
	if s := strings.TrimSpace(string(data)); strings.HasPrefix(strings.ToLower(s), "error") {
		return nil, errors.New(s)
	}
	return data, nil
}

func clamp100(v int) int { return max(-100, min(100, v)) }

// PTZMove starts (or with 0 values stops) continuous movements. pan/tilt/zoom/focus are
// speeds from -100 to 100 (pan right, tilt up, zoom in, focus far are positive). Only the
// non-nil values are sent.
func (c *Client) PTZMove(ctx context.Context, panTilt *[2]int, zoom, focus *int) error {
	q := url.Values{}
	if panTilt != nil {
		q.Set("continuouspantiltmove", fmt.Sprintf("%d,%d", clamp100(panTilt[0]), clamp100(panTilt[1])))
	}
	if zoom != nil {
		q.Set("continuouszoommove", strconv.Itoa(clamp100(*zoom)))
	}
	if focus != nil {
		q.Set("continuousfocusmove", strconv.Itoa(clamp100(*focus)))
	}
	if len(q) == 0 {
		return nil
	}
	_, err := c.ptz(ctx, q)
	return err
}

// PTZStop stops all continuous movements.
func (c *Client) PTZStop(ctx context.Context, caps PTZCaps) error {
	zero := 0
	var pt *[2]int
	var z, f *int
	if caps.PanTilt {
		pt = &[2]int{}
	}
	if caps.Zoom {
		z = &zero
	}
	if caps.Focus {
		f = &zero
	}
	return c.PTZMove(ctx, pt, z, f)
}

// PTZHome moves to the home position.
func (c *Client) PTZHome(ctx context.Context) error {
	_, err := c.ptz(ctx, url.Values{"move": {"home"}})
	return err
}

// PTZAreaZoom centers the image on (x, y) of an image of size w×h and zooms by
// factor zoom/100 (100 = only center, 200 = twice, 50 = half).
func (c *Client) PTZAreaZoom(ctx context.Context, x, y, w, h, zoom int) error {
	_, err := c.ptz(ctx, url.Values{
		"areazoom":    {fmt.Sprintf("%d,%d,%d", x, y, zoom)},
		"imagewidth":  {strconv.Itoa(w)},
		"imageheight": {strconv.Itoa(h)},
	})
	return err
}

// PTZRelZoom zooms relatively (-9999..9999, positive = in).
func (c *Client) PTZRelZoom(ctx context.Context, steps int) error {
	_, err := c.ptz(ctx, url.Values{"rzoom": {strconv.Itoa(max(-9999, min(9999, steps)))}})
	return err
}

// PTZRelFocus moves the focus relatively (-9999..9999, positive = far).
func (c *Client) PTZRelFocus(ctx context.Context, steps int) error {
	_, err := c.ptz(ctx, url.Values{"rfocus": {strconv.Itoa(max(-9999, min(9999, steps)))}})
	return err
}

// PTZPosition is the reply of query=position (only the fields the device reports are set).
type PTZPosition struct {
	Zoom, Focus       int // 1…9999, 0 = unknown
	HasZoom, HasFocus bool
}

// PTZGetPosition reads the current position of channel 1.
func (c *Client) PTZGetPosition(ctx context.Context) (PTZPosition, error) {
	data, err := c.ptz(ctx, url.Values{"query": {"position"}})
	if err != nil {
		return PTZPosition{}, err
	}
	return parsePosition(string(data)), nil
}

func parsePosition(s string) PTZPosition {
	var p PTZPosition
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case "zoom":
			p.Zoom, p.HasZoom = int(f), true
		case "focus":
			p.Focus, p.HasFocus = int(f), true
		}
	}
	return p
}

// PTZSetZoom zooms to an absolute position (1 wide … 9999 tele).
func (c *Client) PTZSetZoom(ctx context.Context, z int) error {
	_, err := c.ptz(ctx, url.Values{"zoom": {strconv.Itoa(max(1, min(9999, z)))}})
	return err
}

// PTZSetFocus moves the focus to an absolute position (1 near … 9999 far).
func (c *Client) PTZSetFocus(ctx context.Context, f int) error {
	_, err := c.ptz(ctx, url.Values{"focus": {strconv.Itoa(max(1, min(9999, f)))}})
	return err
}

// PTZStepZoom zooms one step in or out with whatever the device supports
// (relative, otherwise absolute based on the current position). step is 1…9999.
func (c *Client) PTZStepZoom(ctx context.Context, caps PTZCaps, step int, in bool) error {
	if !in {
		step = -step
	}
	switch {
	case caps.RelZoom:
		return c.PTZRelZoom(ctx, step)
	case caps.AbsZoom:
		pos, err := c.PTZGetPosition(ctx)
		if err != nil {
			return err
		}
		if !pos.HasZoom {
			return errors.New("device reports no zoom position")
		}
		return c.PTZSetZoom(ctx, pos.Zoom+step)
	}
	return ErrNotSupported
}

// PTZStepFocus moves the focus one step far (positive) or near.
func (c *Client) PTZStepFocus(ctx context.Context, caps PTZCaps, step int, far bool) error {
	if !far {
		step = -step
	}
	switch {
	case caps.RelFocus:
		return c.PTZRelFocus(ctx, step)
	case caps.AbsFocus:
		pos, err := c.PTZGetPosition(ctx)
		if err != nil {
			return err
		}
		if !pos.HasFocus {
			return errors.New("device reports no focus position")
		}
		return c.PTZSetFocus(ctx, pos.Focus+step)
	}
	return ErrNotSupported
}

// PTZDiagnostics returns the raw PTZ parameters and the position query (for bug reports).
func (c *Client) PTZDiagnostics(ctx context.Context) string {
	var b strings.Builder
	for _, g := range []string{"Properties.PTZ", "PTZ.Support", "PTZ.Various"} {
		p, err := c.params(ctx, g)
		fmt.Fprintf(&b, "== param.cgi group=%s\n", g)
		if err != nil {
			fmt.Fprintf(&b, "error: %v\n", err)
			continue
		}
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, p[k])
		}
	}
	b.WriteString("== ptz.cgi query=position\n")
	if data, err := c.ptz(ctx, url.Values{"query": {"position"}}); err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		b.WriteString(strings.TrimSpace(string(data)) + "\n")
	}
	b.WriteString("== ptz.cgi info=1\n")
	if data, err := c.ptz(ctx, url.Values{"info": {"1"}}); err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		b.WriteString(strings.TrimSpace(string(data)) + "\n")
	}
	b.WriteString("== apidiscovery.cgi getApiList\n")
	var apis struct {
		APIList []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Name    string `json:"name"`
		} `json:"apiList"`
	}
	if err := c.jsonCall(ctx, "apidiscovery.cgi", "getApiList", nil, true, &apis); err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		for _, a := range apis.APIList {
			fmt.Fprintf(&b, "%s %s (%s)\n", a.ID, a.Version, a.Name)
		}
	}
	for _, g := range []string{"Properties.API", "Properties.Image", "ImageSource.I0"} {
		p, err := c.params(ctx, g)
		fmt.Fprintf(&b, "== param.cgi group=%s\n", g)
		if err != nil {
			fmt.Fprintf(&b, "error: %v\n", err)
			continue
		}
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, p[k])
		}
	}
	for _, path := range []string{"/axis-cgi/opticssetup.cgi?monitor=poll", "/axis-cgi/opticssetup.cgi"} {
		code, data, err := c.do(ctx, http.MethodGet, path, nil, "", true)
		fmt.Fprintf(&b, "== GET %s → %d %v\n%s\n", path, code, err, strings.TrimSpace(string(data[:min(len(data), 600)])))
	}
	b.WriteString("== opticscontrol.cgi getCapabilities\n")
	if o, err := c.OpticsCapabilities(ctx); err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		fmt.Fprintf(&b, "%+v\n", o)
	}
	return b.String()
}

// PTZAutoFocus switches autofocus on or off.
func (c *Client) PTZAutoFocus(ctx context.Context, on bool) error {
	_, err := c.ptz(ctx, url.Values{"autofocus": {map[bool]string{true: "on", false: "off"}[on]}})
	return err
}

// PTZPresets lists the server presets of channel 1, sorted by number.
func (c *Client) PTZPresets(ctx context.Context) ([]PTZPreset, error) {
	data, err := c.ptz(ctx, url.Values{"query": {"presetposcam"}})
	if err != nil {
		return nil, err
	}
	return parsePresets(string(data)), nil
}

// parsePresets reads lines like "presetposno1=Home".
func parsePresets(s string) []PTZPreset {
	var out []PTZPreset
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(k, "presetposno") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(k, "presetposno")); err == nil {
			out = append(out, PTZPreset{No: n, Name: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].No < out[j].No })
	return out
}

// PTZGotoPreset moves to a server preset.
func (c *Client) PTZGotoPreset(ctx context.Context, no int) error {
	_, err := c.ptz(ctx, url.Values{"gotoserverpresetno": {strconv.Itoa(no)}})
	return err
}

// ---------------------------------------------------------------- Optics

// Motorized lenses of fixed cameras are controlled with one of two APIs:
//   - opticscontrol.cgi (Optics Control API, JSON, AXIS OS 10+): magnification 1…max
//   - opticssetup.cgi (older firmware, e.g. P3225-VE Mk II with 9.80): query parameters,
//     zoom and focus as positions 0…1, state as XML. This is what the device's own web
//     interface uses there ("Focus & zoom").
// The methods below take an Optics and use the right API.

// Optics describes a motorized lens of a fixed camera.
type Optics struct {
	ID               string
	Legacy           bool // opticssetup.cgi instead of opticscontrol.cgi
	Zoom, Focus      bool
	AutoFocus        bool
	CalibrateZoom    bool
	CalibrateFocus   bool
	MaxMagnification float64 // opticscontrol.cgi only; Legacy: zoom is a position 0…1
}

// CanCalibrate: the lens supports calibrating zoom and/or focus.
func (o Optics) CanCalibrate() bool { return o.CalibrateZoom || o.CalibrateFocus }

// OpticsState is the current lens position.
type OpticsState struct {
	Focus         float64 // 0 (near) … 1 (far)
	FocusMoving   bool
	Magnification float64 // opticscontrol.cgi: 1 … MaxMagnification, 0 = no zoom
	ZoomPos       float64 // opticssetup.cgi: 0 (wide) … 1 (tele)
	ZoomMoving    bool
}

// Moving: zoom or focus motor is running (e.g. autofocus, calibration).
func (s OpticsState) Moving() bool { return s.FocusMoving || s.ZoomMoving }

func (c *Client) optics(ctx context.Context, method string, params any, out any) error {
	err := c.jsonCallVersion(ctx, "opticscontrol.cgi", "1", method, params, true, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 2105 {
		// known firmware behavior: admin methods fail while anonymous viewing is allowed
		return fmt.Errorf("%w – disable \"Allow anonymous viewers\" on the device", err)
	}
	return err
}

func opticsParams(id string, extra map[string]any) map[string]any {
	o := map[string]any{"opticsId": id}
	for k, v := range extra {
		o[k] = v
	}
	return map[string]any{"optics": []map[string]any{o}}
}

// opticsSetup sends an opticssetup.cgi command. The query is passed as is: the device
// expects a literal "+" in "+minstep" (like its web interface sends it).
func (c *Client) opticsSetup(ctx context.Context, query string) ([]byte, error) {
	code, data, err := c.do(ctx, http.MethodGet, "/axis-cgi/opticssetup.cgi?"+query, nil, "", true)
	if err != nil {
		if code == http.StatusServiceUnavailable {
			return nil, errors.New("lens busy or camera temperature out of range – try again later")
		}
		if code == http.StatusBadRequest {
			return nil, fmt.Errorf("opticssetup.cgi: %s rejected", query)
		}
		return nil, err
	}
	if strings.Contains(strings.ToLower(string(data)), "error") {
		return nil, fmt.Errorf("opticssetup.cgi: %s", strings.TrimSpace(string(data)))
	}
	return data, nil
}

var opticsAttrRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// parseOpticsSetup reads <opticsSetupState focusPosition="0.28" zoomPosition="0" …/>.
func parseOpticsSetup(xml string) (OpticsState, map[string]bool, bool) {
	i := strings.Index(xml, "<opticsSetupState")
	if i < 0 {
		return OpticsState{}, nil, false
	}
	tag := xml[i:]
	if j := strings.Index(tag, ">"); j >= 0 {
		tag = tag[:j]
	}
	var st OpticsState
	has := map[string]bool{}
	for _, m := range opticsAttrRe.FindAllStringSubmatch(tag, -1) {
		f, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		has[m[1]] = true
		switch m[1] {
		case "focusPosition":
			st.Focus = f
		case "zoomPosition":
			st.ZoomPos = f
		case "focusOperationInProgress":
			st.FocusMoving = f != 0
		case "zoomOperationInProgress":
			st.ZoomMoving = f != 0
		}
	}
	return st, has, true
}

// opticsSetupCapabilities detects a lens controllable via opticssetup.cgi.
func (c *Client) opticsSetupCapabilities(ctx context.Context) (*Optics, error) {
	data, err := c.opticsSetup(ctx, "monitor=poll")
	if err != nil {
		return nil, err
	}
	_, has, ok := parseOpticsSetup(string(data))
	if !ok || !has["focusPosition"] {
		return nil, nil
	}
	o := &Optics{ID: "opticssetup", Legacy: true, Focus: true, AutoFocus: true, CalibrateFocus: true}
	if has["zoomPosition"] {
		o.Zoom, o.CalibrateZoom = true, true
	}
	return o, nil
}

// OpticsCapabilities lists the controllable lenses. Devices without the API return
// ErrNotSupported, devices without motorized lens an empty list.
func (c *Client) OpticsCapabilities(ctx context.Context) ([]Optics, error) {
	var d struct {
		Optics []struct {
			OpticsID         string   `json:"opticsId"`
			Capabilities     []string `json:"capabilities"`
			MaxMagnification float64  `json:"maxMagnification"`
		} `json:"optics"`
	}
	err := c.optics(ctx, "getCapabilities", nil, &d)
	if errors.Is(err, ErrNotSupported) {
		o, lerr := c.opticsSetupCapabilities(ctx)
		if lerr != nil || o == nil {
			if errors.Is(lerr, ErrAuth) {
				return nil, lerr
			}
			return nil, err
		}
		return []Optics{*o}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Optics
	for _, o := range d.Optics {
		x := Optics{ID: o.OpticsID, MaxMagnification: o.MaxMagnification}
		for _, cp := range o.Capabilities {
			switch cp {
			case "zoom":
				x.Zoom = true
			case "focus":
				x.Focus, x.AutoFocus = true, true
			case "calibrateZoom":
				x.CalibrateZoom = true
			case "calibrateFocus":
				x.CalibrateFocus = true
			}
		}
		if x.Zoom && x.MaxMagnification < 1 {
			x.MaxMagnification = 1
		}
		if x.Zoom || x.Focus || x.CanCalibrate() {
			out = append(out, x)
		}
	}
	return out, nil
}

// GetOptics reads the current lens position.
func (c *Client) GetOptics(ctx context.Context, o Optics) (OpticsState, error) {
	if o.Legacy {
		data, err := c.opticsSetup(ctx, "monitor=poll")
		if err != nil {
			return OpticsState{}, err
		}
		st, _, ok := parseOpticsSetup(string(data))
		if !ok {
			return OpticsState{}, errors.New("opticssetup.cgi: unexpected reply")
		}
		return st, nil
	}
	var d struct {
		Optics []struct {
			OpticsID      string  `json:"opticsId"`
			FocusPosition float64 `json:"focusPosition"`
			FocusMoving   bool    `json:"focusMoving"`
			Magnification float64 `json:"magnification"`
			ZoomMoving    bool    `json:"zoomMoving"`
		} `json:"optics"`
	}
	if err := c.optics(ctx, "getOptics", nil, &d); err != nil {
		return OpticsState{}, err
	}
	for _, x := range d.Optics {
		if x.OpticsID == o.ID {
			return OpticsState{Focus: x.FocusPosition, FocusMoving: x.FocusMoving,
				Magnification: x.Magnification, ZoomMoving: x.ZoomMoving}, nil
		}
	}
	return OpticsState{}, fmt.Errorf("optics %q not found", o.ID)
}

func fmtPos(v float64) string { return strconv.FormatFloat(max(0, min(1, v)), 'f', 4, 64) }

// SetZoom zooms to an absolute value: the magnification (1 … MaxMagnification) with
// opticscontrol.cgi, the position 0 (wide) … 1 (tele) with opticssetup.cgi (Legacy).
func (c *Client) SetZoom(ctx context.Context, o Optics, v float64) error {
	if o.Legacy {
		_, err := c.opticsSetup(ctx, "azoom="+fmtPos(v))
		return err
	}
	return c.optics(ctx, "setMagnification", opticsParams(o.ID, map[string]any{"magnification": max(1, v)}), nil)
}

// SetFocus moves the focus to an absolute position (0 near … 1 far).
func (c *Client) SetFocus(ctx context.Context, o Optics, pos float64) error {
	if o.Legacy {
		_, err := c.opticsSetup(ctx, "afocus="+fmtPos(pos))
		return err
	}
	return c.optics(ctx, "setFocus", opticsParams(o.ID, map[string]any{"position": max(0, min(1, pos))}), nil)
}

// Relative step sizes for StepZoom/StepFocus.
const (
	StepSmall = "smallStep"
	StepBig   = "bigStep"
)

func stepType(step string, positive bool, legacy bool) string {
	if legacy {
		step = map[string]string{StepSmall: "minstep", StepBig: "bigstep"}[step]
	}
	if positive {
		return "+" + step
	}
	return "-" + step
}

// StepZoom zooms in (positive) or out by a small or big step.
func (c *Client) StepZoom(ctx context.Context, o Optics, step string, positive bool) error {
	if o.Legacy {
		_, err := c.opticsSetup(ctx, "rzoom="+stepType(step, positive, true))
		return err
	}
	return c.optics(ctx, "setRelativeMagnification", opticsParams(o.ID, map[string]any{"type": stepType(step, positive, false)}), nil)
}

// StepFocus moves the focus far (positive) or near by a small or big step.
func (c *Client) StepFocus(ctx context.Context, o Optics, step string, positive bool) error {
	if o.Legacy {
		_, err := c.opticsSetup(ctx, "rfocus="+stepType(step, positive, true))
		return err
	}
	return c.optics(ctx, "setRelativeFocus", opticsParams(o.ID, map[string]any{"type": stepType(step, positive, false)}), nil)
}

// Autofocus starts the autofocus; the lens reports FocusMoving until it is done.
func (c *Client) Autofocus(ctx context.Context, o Optics) error {
	if o.Legacy {
		// nonblocking: otherwise the reply only comes when the lens is done
		_, err := c.opticsSetup(ctx, "autofocus=perform&nonblocking=yes")
		return err
	}
	return c.optics(ctx, "performAutofocus", opticsParams(o.ID, nil), nil)
}

// CalibrateOptics recalibrates the reference positions of the zoom and/or focus motor
// (e.g. after the lens was moved by hand or the image is permanently out of focus).
// Unsupported parts are skipped.
func (c *Client) CalibrateOptics(ctx context.Context, o Optics) error {
	if !o.CanCalibrate() {
		return ErrNotSupported
	}
	if o.Legacy {
		var parts []string
		if o.CalibrateFocus {
			parts = append(parts, "focus")
		}
		if o.CalibrateZoom {
			parts = append(parts, "zoom")
		}
		// nonblocking: the calibration takes ~50 s (P3225-VE Mk II); progress via GetOptics
		_, err := c.opticsSetup(ctx, "calibrate="+strings.Join(parts, ",")+"&nonblocking=yes")
		return err
	}
	return c.optics(ctx, "calibrate", opticsParams(o.ID, map[string]any{
		"zoom": o.CalibrateZoom, "focus": o.CalibrateFocus}), nil)
}

// ---------------------------------------------------------------- Combined

// LensControls is everything the live view can control on a device.
type LensControls struct {
	PTZ     PTZCaps
	Presets []PTZPreset
	Optics  *Optics // first motorized lens (opticscontrol.cgi or opticssetup.cgi), nil = none
}

// Any reports whether there is anything to control.
func (l LensControls) Any() bool { return l.PTZ.Any() || l.Optics != nil }

// GetLensControls determines PTZ and lens support. Missing APIs are not an error;
// only authentication errors are returned.
func (c *Client) GetLensControls(ctx context.Context) (LensControls, error) {
	var l LensControls
	caps, err := c.PTZCapabilities(ctx)
	if errors.Is(err, ErrAuth) {
		return l, err
	}
	l.PTZ = caps
	if caps.Presets {
		l.Presets, _ = c.PTZPresets(ctx)
	}
	opt, err := c.OpticsCapabilities(ctx)
	if errors.Is(err, ErrAuth) {
		return l, err
	}
	if len(opt) > 0 {
		l.Optics = &opt[0]
	}
	return l, nil
}
