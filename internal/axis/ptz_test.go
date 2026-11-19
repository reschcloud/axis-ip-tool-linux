// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func loggedIn(t *testing.T) (*fakeCamera, *Client) {
	f, host, port := startFake(t)
	f.users["root"] = "geheim123"
	return f, NewClient(host, port, "root", "geheim123")
}

func TestPTZ(t *testing.T) {
	ctx := context.Background()
	f, c := loggedIn(t)

	if caps, err := c.PTZCapabilities(ctx); err != nil || caps.Any() {
		t.Fatalf("fixed camera: %+v, %v", caps, err)
	}
	f.ptz = true
	l, err := c.GetLensControls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !l.PTZ.PanTilt || !l.PTZ.Zoom || l.PTZ.Focus || !l.PTZ.AreaZoom || !l.PTZ.Presets {
		t.Fatalf("caps = %+v", l.PTZ)
	}
	if len(l.Presets) != 2 || l.Presets[0] != (PTZPreset{1, "Home"}) || l.Presets[1].Name != "Door" {
		t.Fatalf("presets = %+v", l.Presets)
	}

	zoom := 150 // clamped to 100
	if err := c.PTZMove(ctx, &[2]int{-30, 40}, &zoom, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZStop(ctx, l.PTZ); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZAreaZoom(ctx, 640, 360, 1280, 720, 100); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZGotoPreset(ctx, 3); err != nil {
		t.Fatal(err)
	}
	q := strings.Join(f.ptzQueries, "\n")
	for _, want := range []string{
		"continuouspantiltmove=-30,40", "continuouszoommove=100",
		"continuouspantiltmove=0,0", "continuouszoommove=0",
		"areazoom=640,360,100", "imagewidth=1280", "gotoserverpresetno=3", "camera=1",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in\n%s", want, q)
		}
	}
	if strings.Contains(q, "continuousfocusmove") {
		t.Error("focus stop sent although not supported")
	}
}

func TestOptics(t *testing.T) {
	ctx := context.Background()
	f, c := loggedIn(t)

	l, err := c.GetLensControls(ctx)
	if err != nil || l.Optics == nil {
		t.Fatalf("GetLensControls = %+v, %v", l, err)
	}
	o := *l.Optics
	if o.ID != "0" || !o.Zoom || !o.Focus || !o.CanCalibrate() || o.MaxMagnification != 3.5 {
		t.Fatalf("optics = %+v", o)
	}
	st, err := c.GetOptics(ctx, o)
	if err != nil || st.Focus != 0.25 || st.Magnification != 2 || !st.Moving() {
		t.Fatalf("GetOptics = %+v, %v", st, err)
	}
	if err := c.SetZoom(ctx, o, 2.5); err != nil {
		t.Fatal(err)
	}
	if err := c.StepFocus(ctx, o, StepSmall, false); err != nil {
		t.Fatal(err)
	}
	if err := c.CalibrateOptics(ctx, o); err != nil {
		t.Fatal(err)
	}
	last := f.optics[len(f.optics)-1]
	if last["method"] != "calibrate" || last["apiVersion"] != "1" {
		t.Fatalf("last request %v", last)
	}
	p := last["params"].(map[string]any)["optics"].([]any)[0].(map[string]any)
	if p["opticsId"] != "0" || p["zoom"] != true || p["focus"] != true {
		t.Fatalf("calibrate params %v", p)
	}
	step := f.optics[len(f.optics)-2]["params"].(map[string]any)["optics"].([]any)[0].(map[string]any)
	if step["type"] != "-smallStep" {
		t.Fatalf("step params %v", step)
	}
	err = c.SetFocus(ctx, o, 0.5)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 2105 || !strings.Contains(err.Error(), "anonymous") {
		t.Fatalf("2105 not explained: %v", err)
	}
}

func TestStream(t *testing.T) {
	f, c := loggedIn(t)
	f.frames = 3
	var got [][]byte
	err := c.Stream(context.Background(), StreamOptions{Width: 1280, FPS: 10}, func(b []byte) {
		got = append(got, b)
	})
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Stream = %v", err)
	}
	if len(got) != 3 || got[2][2] != 2 || !isJPEG(got[0]) {
		t.Fatalf("frames = %v", got)
	}
}

func TestReadMJPEGWithoutLength(t *testing.T) {
	body := "--b\r\nContent-Type: image/jpeg\r\n\r\n\xff\xd8\x01\xff\xd9\r\n" +
		"--b\r\nContent-Type: image/jpeg\r\n\r\n\xff\xd8\x02\n\xff\xd9\r\n--b--\r\n"
	var got []string
	err := readMJPEG(context.Background(), "multipart/x-mixed-replace;boundary=--b",
		strings.NewReader(body), func(b []byte) { got = append(got, string(b)) })
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 || got[0] != "\xff\xd8\x01\xff\xd9" || got[1] != "\xff\xd8\x02\n\xff\xd9" {
		t.Fatalf("frames = %q", got)
	}
	if err := readMJPEG(context.Background(), "image/jpeg", strings.NewReader(""), nil); err == nil {
		t.Fatal("non-multipart accepted")
	}
}

func TestParsePresets(t *testing.T) {
	p := parsePresets("Preset Positions for camera 1\r\npresetposno10=Gate\r\npresetposno2=Home\r\nfoo=bar\n")
	if len(p) != 2 || p[0].No != 2 || p[1].Name != "Gate" {
		t.Fatalf("%+v", p)
	}
}

func TestPTZStepFallbacks(t *testing.T) {
	ctx := context.Background()
	f, c := loggedIn(t)

	caps := parsePTZSupport(map[string]string{
		"PTZ.Support.S1.AbsoluteZoom": "true", "PTZ.Support.S1.RelativeFocus": "true",
	})
	if !caps.AnyZoom() || caps.Zoom || !caps.AnyFocus() || caps.Focus || !caps.Any() {
		t.Fatalf("caps = %+v", caps)
	}
	if p := parsePosition("pan=0\nzoom=1200.5\nfocus=4000\nautofocus=on\n"); !p.HasZoom || p.Zoom != 1200 || p.Focus != 4000 {
		t.Fatalf("position = %+v", p)
	}
	// absolute zoom: without a reported position it must fail …
	if err := c.PTZStepZoom(ctx, caps, 500, true); err == nil {
		t.Fatal("expected error without zoom position")
	}
	// … with one it zooms relative to it
	f.ptz = true
	if err := c.PTZStepZoom(ctx, caps, 500, true); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZStepFocus(ctx, caps, 300, false); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZStepZoom(ctx, PTZCaps{RelZoom: true}, 500, false); err != nil {
		t.Fatal(err)
	}
	if err := c.PTZStepZoom(ctx, PTZCaps{}, 500, false); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v", err)
	}
	q := strings.Join(f.ptzQueries, "\n")
	for _, want := range []string{"query=position", "zoom=501", "rfocus=-300", "rzoom=-500"} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in\n%s", want, q)
		}
	}
}

// P3225-VE Mk II with 9.80: no opticscontrol.cgi, lens via opticssetup.cgi.
func TestOpticsSetup(t *testing.T) {
	ctx := context.Background()
	f, c := loggedIn(t)
	f.opticsSetup = true

	l, err := c.GetLensControls(ctx)
	if err != nil || l.Optics == nil {
		t.Fatalf("lens = %+v, %v", l, err)
	}
	o := *l.Optics
	if !o.Legacy || !o.Zoom || !o.Focus || !o.AutoFocus || !o.CalibrateZoom || !o.CalibrateFocus {
		t.Fatalf("optics = %+v", o)
	}
	st, err := c.GetOptics(ctx, o)
	if err != nil || st.Focus != 0.281664 || st.ZoomPos != 0.15 || !st.FocusMoving || st.ZoomMoving {
		t.Fatalf("state = %+v, %v", st, err)
	}
	for _, fn := range []func() error{
		func() error { return c.SetZoom(ctx, o, 0.5) },
		func() error { return c.SetFocus(ctx, o, 1.7) },
		func() error { return c.StepZoom(ctx, o, StepSmall, true) },
		func() error { return c.StepFocus(ctx, o, StepBig, false) },
		func() error { return c.Autofocus(ctx, o) },
		func() error { return c.CalibrateOptics(ctx, o) },
	} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	q := strings.Join(f.opticsSetupQueries, "\n")
	for _, want := range []string{"azoom=0.5000", "afocus=1.0000", "rzoom=+minstep", "rfocus=-bigstep",
		"autofocus=perform&nonblocking=yes", "calibrate=focus,zoom&nonblocking=yes"} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in\n%s", want, q)
		}
	}
}
