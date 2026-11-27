// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"ip-tool-for-axis/internal/axis"
)

// Live view: MJPEG stream (snapshot polling as fallback) with a sidebar for PTZ,
// zoom/focus and lens calibration – depending on what the device supports.

const (
	previewWidth = 1280
	streamFPS    = 15
)

type liveView struct {
	w   *window
	c   *axis.Client
	dlg *adw.Dialog

	ctx    context.Context // dialog lifetime
	cancel context.CancelFunc

	pic    *gtk.Picture
	status *gtk.Label
	toasts *adw.ToastOverlay
	play   *gtk.ToggleButton

	streamCancel context.CancelFunc
	frameBusy    atomic.Bool // a frame is waiting for the GTK thread → drop newer ones
	frameW       int
	frameH       int
	mode         string // "live" or "snapshot"
	fpsCount     int
	fpsSince     time.Time
	fps          int

	cmds chan func(context.Context) error // ordered device commands (start/stop!)

	lens    axis.LensControls
	speed   *gtk.Scale
	zoom    *gtk.Scale // optics magnification
	focus   *gtk.Scale // optics focus position (0…100 %)
	moving  *gtk.Label
	syncing bool      // sliders are set programmatically
	touched time.Time // last user change of a slider
	polling bool
	calib   bool // calibration running
}

// previewStart is what the live view needs before it opens: a first image and the
// device's PTZ/lens capabilities (so the controls are there right away).
type previewStart struct {
	img  []byte
	lens axis.LensControls
}

func loadPreview(ctx context.Context, c *axis.Client) (previewStart, error) {
	img, err := c.Snapshot(ctx, previewWidth)
	if err != nil {
		return previewStart{}, err
	}
	// Lens/optics info is optional: some accounts may see the image but not the PTZ API.
	lens, _ := c.GetLensControls(ctx)
	return previewStart{img: img, lens: lens}, nil
}

// loadPreviewAnonymous loads a first image without credentials. Lens controls are
// optional: anonymous viewers often may not query them.
func loadPreviewAnonymous(ctx context.Context, c *axis.Client) (previewStart, error) {
	img, err := c.Snapshot(ctx, previewWidth)
	if err != nil {
		return previewStart{}, err
	}
	lens, _ := c.GetLensControls(ctx)
	return previewStart{img: img, lens: lens}, nil
}

func (w *window) previewDialog(d *axis.Device, start previewStart) {
	creds := w.creds[d.Key()]
	lv := &liveView{w: w, c: d.Client(creds[0], creds[1]),
		cmds: make(chan func(context.Context) error, 64), lens: start.lens}
	lv.ctx, lv.cancel = context.WithCancel(context.Background())
	lv.build(d)
	lv.showFrame(start.img)
	lv.status.SetText("Connecting to live stream…")
	go lv.worker()
	lv.startStream()
	if lv.lens.Any() {
		lv.pictureGestures()
		lv.pollOptics()
	}
	glib.TimeoutAdd(1000, func() bool {
		if lv.ctx.Err() != nil {
			return false
		}
		lv.pollOptics()
		return true
	})
	lv.dlg.Present(w)
}

func (lv *liveView) build(d *axis.Device) {
	lv.dlg = adw.NewDialog()
	lv.dlg.SetTitle("Live view – " + label(d))
	lv.dlg.SetContentWidth(1200)
	lv.dlg.SetContentHeight(720)

	lv.pic = gtk.NewPicture()
	lv.pic.SetContentFit(gtk.ContentFitContain)
	lv.pic.SetCanShrink(true)
	lv.pic.SetVExpand(true)
	lv.pic.SetHExpand(true)

	lv.status = gtk.NewLabel("")
	lv.status.AddCSSClass("dim-label")
	lv.status.SetEllipsize(pango.EllipsizeEnd)

	lv.play = gtk.NewToggleButton()
	lv.play.SetIconName("media-playback-pause-symbolic")
	lv.play.SetTooltipText("Pause the live image")
	lv.play.SetActive(true)
	lv.play.ConnectToggled(func() {
		if lv.play.Active() {
			lv.play.SetIconName("media-playback-pause-symbolic")
			lv.play.SetTooltipText("Pause the live image")
			lv.startStream()
		} else {
			lv.play.SetIconName("media-playback-start-symbolic")
			lv.play.SetTooltipText("Resume the live image")
			lv.stopStream()
			lv.status.SetText("Paused")
		}
	})

	hb := adw.NewHeaderBar()
	hb.PackStart(lv.play)
	bar := gtk.NewActionBar()
	bar.SetCenterWidget(lv.status)

	// the controls are always visible next to the image when the device has any
	content := gtk.NewBox(gtk.OrientationHorizontal, 0)
	content.Append(lv.pic)
	if lv.lens.Any() {
		content.Append(gtk.NewSeparator(gtk.OrientationVertical))
		side := lv.sidebar()
		side.SetSizeRequest(290, -1)
		content.Append(side)
	}

	lv.toasts = adw.NewToastOverlay()
	lv.toasts.SetChild(content)

	tv := adw.NewToolbarView()
	tv.AddTopBar(hb)
	tv.SetContent(lv.toasts)
	tv.AddBottomBar(bar)
	lv.dlg.SetChild(tv)

	lv.dlg.ConnectClosed(func() {
		lv.cancel()
		lv.stopStream()
		if p := lv.lens.PTZ; p.PanTilt || p.Zoom || p.Focus {
			// never leave the camera moving (button released outside the window etc.)
			go func(c *axis.Client) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c.PTZStop(ctx, p)
			}(lv.c)
		}
	})
}

func (lv *liveView) toast(msg string) {
	t := adw.NewToast(msg)
	t.SetTimeout(5)
	lv.toasts.AddToast(t)
}

// ---------------------------------------------------------------- Image

func (lv *liveView) showFrame(img []byte) {
	tex, err := gdk.NewTextureFromBytes(glib.NewBytes(img))
	if err != nil {
		lv.status.SetText("Image not readable: " + err.Error())
		return
	}
	lv.pic.SetPaintable(tex)
	lv.frameW, lv.frameH = tex.Width(), tex.Height()
	lv.fpsCount++
	if el := time.Since(lv.fpsSince); el >= time.Second {
		lv.fps = int(float64(lv.fpsCount)/el.Seconds() + 0.5)
		lv.fpsCount, lv.fpsSince = 0, time.Now()
	}
	switch lv.mode {
	case "live":
		lv.status.SetText(fmt.Sprintf("%d × %d · %d fps · live", lv.frameW, lv.frameH, lv.fps))
	default:
		lv.status.SetText(fmt.Sprintf("%d × %d · still image as of %s", lv.frameW, lv.frameH,
			time.Now().Format("15:04:05")))
	}
}

func (lv *liveView) stopStream() {
	if lv.streamCancel != nil {
		lv.streamCancel()
		lv.streamCancel = nil
	}
}

// startStream receives the MJPEG stream and reconnects after interruptions. If the
// device offers no stream, it falls back to one still image per second.
func (lv *liveView) startStream() {
	lv.stopStream()
	ctx, cancel := context.WithCancel(lv.ctx)
	lv.streamCancel = cancel
	ui := func(f func()) {
		glib.IdleAdd(func() {
			if ctx.Err() == nil {
				f()
			}
		})
	}
	go func() {
		for ctx.Err() == nil {
			err := lv.c.Stream(ctx, axis.StreamOptions{Width: previewWidth, FPS: streamFPS}, func(b []byte) {
				if !lv.frameBusy.CompareAndSwap(false, true) {
					return // GTK thread is behind: drop the frame
				}
				glib.IdleAdd(func() {
					lv.frameBusy.Store(false)
					if ctx.Err() == nil {
						lv.mode = "live"
						lv.showFrame(b)
					}
				})
			})
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, axis.ErrAuth):
				ui(func() { lv.status.SetText(err.Error()) })
				return
			case errors.Is(err, axis.ErrNotSupported):
				lv.pollSnapshots(ctx, ui)
				return
			}
			ui(func() { lv.status.SetText(fmt.Sprintf("Stream interrupted (%v) – reconnecting…", err)) })
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

// pollSnapshots is the fallback for devices without MJPEG: one still image per second.
func (lv *liveView) pollSnapshots(ctx context.Context, ui func(func())) {
	for ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		img, err := lv.c.Snapshot(sctx, previewWidth)
		cancel()
		ui(func() {
			lv.mode = "snapshot"
			if err != nil {
				lv.status.SetText(err.Error())
			} else {
				lv.showFrame(img)
			}
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// imagePos converts widget coordinates of the picture into image pixels.
func (lv *liveView) imagePos(x, y float64) (int, int, bool) {
	pw, ph := float64(lv.pic.Width()), float64(lv.pic.Height())
	fw, fh := float64(lv.frameW), float64(lv.frameH)
	if fw == 0 || fh == 0 || pw == 0 || ph == 0 {
		return 0, 0, false
	}
	s := min(pw/fw, ph/fh) // ContentFitContain
	ix, iy := (x-(pw-fw*s)/2)/s, (y-(ph-fh*s)/2)/s
	if ix < 0 || iy < 0 || ix >= fw || iy >= fh {
		return 0, 0, false
	}
	return int(ix), int(iy), true
}

// ---------------------------------------------------------------- Commands

// worker executes device commands strictly one after another, so a "stop" can never
// overtake the corresponding "start".
func (lv *liveView) worker() {
	for {
		select {
		case <-lv.ctx.Done():
			return
		case f := <-lv.cmds:
			ctx, cancel := context.WithTimeout(lv.ctx, 10*time.Second)
			err := f(ctx)
			cancel()
			if err != nil && lv.ctx.Err() == nil {
				glib.IdleAdd(func() {
					if lv.ctx.Err() == nil {
						lv.toast(err.Error())
					}
				})
			}
		}
	}
}

func (lv *liveView) run(f func(ctx context.Context, c *axis.Client) error) {
	c := lv.c
	cmd := func(ctx context.Context) error { return f(ctx, c) }
	select {
	case lv.cmds <- cmd:
		return
	default:
	}
	// Queue full: the device doesn't keep up. Drop the oldest command so that a
	// current stop is not silently lost behind stale moves.
	select {
	case <-lv.cmds:
	default:
	}
	select {
	case lv.cmds <- cmd:
	default:
	}
}

// ---------------------------------------------------------------- Controls

// holdButton starts an action on press and stops it on release (continuous moves).
func holdButton(child gtk.Widgetter, tooltip string, start, stop func()) *gtk.Button {
	b := gtk.NewButton()
	b.SetChild(child)
	b.SetTooltipText(tooltip)
	g := gtk.NewGestureDrag()
	g.SetPropagationPhase(gtk.PhaseCapture)
	active := false
	end := func() {
		if active {
			active = false
			b.RemoveCSSClass("suggested-action")
			stop()
		}
	}
	g.ConnectDragBegin(func(float64, float64) {
		g.SetState(gtk.EventSequenceClaimed)
		if !active {
			active = true
			b.AddCSSClass("suggested-action") // visible "pressed" state
			start()
		}
	})
	g.ConnectDragEnd(func(float64, float64) { end() })
	g.ConnectCancel(func(*gdk.EventSequence) { end() })
	b.AddController(g)
	return b
}

func iconChild(name string) gtk.Widgetter { return gtk.NewImageFromIconName(name) }

func textChild(s string) gtk.Widgetter {
	l := gtk.NewLabel(s)
	l.AddCSSClass("title-3")
	return l
}

func flatButton(icon, tooltip string, cb func()) *gtk.Button {
	b := gtk.NewButtonFromIconName(icon)
	b.SetTooltipText(tooltip)
	b.ConnectClicked(cb)
	return b
}

func row(widgets ...gtk.Widgetter) *gtk.Box {
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	for _, w := range widgets {
		box.Append(w)
	}
	return box
}

func (lv *liveView) speedValue() int {
	if lv.speed == nil {
		return 50
	}
	return int(lv.speed.Value())
}

func (lv *liveView) sidebar() *gtk.ScrolledWindow {
	box := gtk.NewBox(gtk.OrientationVertical, 12)
	for _, f := range []func(int){box.SetMarginTop, box.SetMarginBottom, box.SetMarginStart, box.SetMarginEnd} {
		f(12)
	}
	p, o := lv.lens.PTZ, lv.lens.Optics
	opticsZoom := o != nil && o.Zoom
	opticsFocus := o != nil && o.Focus

	// speed only for continuous ptz.cgi moves (the optics API takes positions)
	if p.PanTilt || (p.AnyZoom() && !opticsZoom) || (p.AnyFocus() && !opticsFocus) {
		lv.speed = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, 100, 1)
		lv.speed.SetValue(50)
		lv.speed.SetHExpand(true)
		lv.speed.SetDrawValue(true)
		lv.speed.SetDigits(0)
	}

	// pan/tilt
	if p.PanTilt {
		box.Append(heading("Pan / tilt"))
		grid := gtk.NewGrid()
		grid.SetRowSpacing(6)
		grid.SetColumnSpacing(6)
		grid.SetHAlign(gtk.AlignCenter)
		for _, dir := range []struct {
			text       string
			col, row   int
			pan, tilt  int
			tooltipDir string
		}{
			{"↖", 0, 0, -1, 1, "up left"}, {"↑", 1, 0, 0, 1, "up"}, {"↗", 2, 0, 1, 1, "up right"},
			{"←", 0, 1, -1, 0, "left"}, {"→", 2, 1, 1, 0, "right"},
			{"↙", 0, 2, -1, -1, "down left"}, {"↓", 1, 2, 0, -1, "down"}, {"↘", 2, 2, 1, -1, "down right"},
		} {
			dir := dir
			b := holdButton(textChild(dir.text), "Move "+dir.tooltipDir+" (hold)", func() {
				s := lv.speedValue()
				pt := [2]int{dir.pan * s, dir.tilt * s}
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, &pt, nil, nil) })
			}, func() {
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, &[2]int{}, nil, nil) })
			})
			b.SetSizeRequest(52, 52)
			grid.Attach(b, dir.col, dir.row, 1, 1)
		}
		home := flatButton("go-home-symbolic", "Home position", func() {
			lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZHome(ctx) })
		})
		home.SetSizeRequest(52, 52)
		grid.Attach(home, 1, 1, 1, 1)
		box.Append(grid)
		if p.AreaZoom {
			box.Append(note("Click in the image to center it there, double-click to zoom in, " +
				"right-click to zoom out."))
		}
	}
	if lv.speed != nil {
		box.Append(row(gtk.NewLabel("Speed"), lv.speed))
	}

	// zoom
	switch {
	case opticsZoom:
		box.Append(heading("Zoom"))
		lens := *o
		if lens.Legacy { // opticssetup.cgi: zoom position 0…100 % (wide … tele)
			lv.zoom = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 0, 100, 0.5)
			lv.zoom.SetFormatValueFunc(func(_ *gtk.Scale, v float64) string { return fmt.Sprintf("%.0f %%", v) })
		} else {
			lv.zoom = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 1, o.MaxMagnification, 0.1)
			lv.zoom.SetFormatValueFunc(func(_ *gtk.Scale, v float64) string { return fmt.Sprintf("%.1f×", v) })
		}
		lv.zoom.SetHExpand(true)
		lv.zoom.SetDrawValue(true)
		lv.onSlider(lv.zoom, func(v float64) {
			if lens.Legacy {
				v /= 100
			}
			lv.run(func(ctx context.Context, c *axis.Client) error { return c.SetZoom(ctx, lens, v) })
		})
		box.Append(row(
			flatButton("zoom-out-symbolic", "Zoom out a step", func() { lv.stepZoom(false) }),
			lv.zoom,
			flatButton("zoom-in-symbolic", "Zoom in a step", func() { lv.stepZoom(true) })))
	case p.Zoom:
		box.Append(heading("Zoom"))
		zoomBtn := func(icon, tip string, sign int) *gtk.Button {
			return holdButton(iconChild(icon), tip, func() {
				z := sign * lv.speedValue()
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, nil, &z, nil) })
			}, func() {
				z := 0
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, nil, &z, nil) })
			})
		}
		r := row(zoomBtn("zoom-out-symbolic", "Zoom out (hold)", -1), zoomBtn("zoom-in-symbolic", "Zoom in (hold)", 1))
		r.SetHAlign(gtk.AlignCenter)
		box.Append(r)
	case p.AnyZoom():
		// no continuous zoom (e.g. fixed cameras with motorized lens): steps
		box.Append(heading("Zoom"))
		r := row(
			flatButton("zoom-out-symbolic", "Zoom out a step", func() { lv.stepZoom(false) }),
			flatButton("zoom-in-symbolic", "Zoom in a step", func() { lv.stepZoom(true) }))
		r.SetHAlign(gtk.AlignCenter)
		box.Append(r)
	}

	// focus
	switch {
	case opticsFocus:
		box.Append(heading("Focus"))
		lv.focus = gtk.NewScaleWithRange(gtk.OrientationHorizontal, 0, 100, 0.5)
		lv.focus.SetHExpand(true)
		lv.focus.SetDrawValue(true)
		lv.focus.SetFormatValueFunc(func(_ *gtk.Scale, v float64) string { return fmt.Sprintf("%.0f %%", v) })
		lens := *o
		lv.onSlider(lv.focus, func(v float64) {
			lv.run(func(ctx context.Context, c *axis.Client) error { return c.SetFocus(ctx, lens, v/100) })
		})
		step := func(far bool) func() {
			return func() {
				lv.touched = time.Now()
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.StepFocus(ctx, lens, axis.StepSmall, far) })
			}
		}
		box.Append(row(flatButton("list-remove-symbolic", "Focus nearer", step(false)), lv.focus,
			flatButton("list-add-symbolic", "Focus farther", step(true))))
		if lens.AutoFocus {
			af := gtk.NewButtonWithLabel("Autofocus")
			af.SetTooltipText("Focus automatically on the image center")
			af.ConnectClicked(func() {
				lv.touched = time.Time{}
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.Autofocus(ctx, lens) })
			})
			box.Append(af)
		}
	case p.AnyFocus() || p.AutoFocus:
		box.Append(heading("Focus"))
		r := row()
		r.SetHAlign(gtk.AlignCenter)
		if !p.Focus && p.AnyFocus() {
			step := func(far bool) func() {
				return func() {
					n := lv.ptzStep()
					lv.run(func(ctx context.Context, c *axis.Client) error {
						return c.PTZStepFocus(ctx, p, n, far)
					})
				}
			}
			r.Append(flatButton("list-remove-symbolic", "Focus nearer", step(false)))
			r.Append(flatButton("list-add-symbolic", "Focus farther", step(true)))
		}
		if p.Focus {
			focusBtn := func(label, tip string, sign int) *gtk.Button {
				return holdButton(gtk.NewLabel(label), tip, func() {
					f := sign * lv.speedValue()
					lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, nil, nil, &f) })
				}, func() {
					f := 0
					lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZMove(ctx, nil, nil, &f) })
				})
			}
			r.Append(focusBtn("Near", "Focus nearer (hold)", -1))
			r.Append(focusBtn("Far", "Focus farther (hold)", 1))
		}
		if p.AutoFocus {
			af := gtk.NewButtonWithLabel("Autofocus")
			af.ConnectClicked(func() {
				lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZAutoFocus(ctx, true) })
			})
			r.Append(af)
		}
		box.Append(r)
	}
	if o != nil {
		lv.moving = note("")
		box.Append(lv.moving)
	}

	// presets
	if len(lv.lens.Presets) > 0 {
		box.Append(heading("Presets"))
		names := make([]string, len(lv.lens.Presets))
		for i, pr := range lv.lens.Presets {
			names[i] = pr.Name
		}
		dd := gtk.NewDropDownFromStrings(names)
		dd.SetHExpand(true)
		gotoBtn := gtk.NewButtonWithLabel("Go")
		gotoBtn.ConnectClicked(func() {
			i := int(dd.Selected())
			if i < 0 || i >= len(lv.lens.Presets) {
				return
			}
			no := lv.lens.Presets[i].No
			lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZGotoPreset(ctx, no) })
		})
		box.Append(row(dd, gotoBtn))
	}

	// lens calibration
	if o != nil && o.CanCalibrate() {
		box.Append(heading("Lens calibration"))
		box.Append(note("Re-establishes the reference positions of the zoom and focus motors, " +
			"e.g. if zoom/focus positions no longer match or the image stays blurred."))
		cal := gtk.NewButtonWithLabel("Calibrate lens…")
		cal.ConnectClicked(func() { lv.confirmCalibration(cal) })
		box.Append(cal)
	}

	sc := gtk.NewScrolledWindow()
	sc.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	sc.SetChild(box)
	return sc
}

// onSlider sends slider changes debounced (only the last value of a drag).
func (lv *liveView) onSlider(s *gtk.Scale, send func(float64)) {
	gen := 0
	s.ConnectValueChanged(func() {
		if lv.syncing {
			return
		}
		lv.touched = time.Now()
		gen++
		g := gen
		glib.TimeoutAdd(250, func() bool {
			if g == gen && lv.ctx.Err() == nil {
				send(s.Value())
			}
			return false
		})
	})
}

func (lv *liveView) stepZoom(in bool) {
	o := lv.lens.Optics
	switch {
	case o != nil && o.Zoom:
		lv.touched = time.Now()
		lens := *o
		lv.run(func(ctx context.Context, c *axis.Client) error {
			return c.StepZoom(ctx, lens, axis.StepSmall, in)
		})
	case lv.lens.PTZ.RelZoom || lv.lens.PTZ.AbsZoom:
		caps, step := lv.lens.PTZ, lv.ptzStep()
		lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZStepZoom(ctx, caps, step, in) })
	}
}

// ptzStep is the step size (of 9999) for relative zoom/focus, scaled by the speed slider.
func (lv *liveView) ptzStep() int { return 10 * lv.speedValue() } // 50 % → 500

// pictureGestures: click to center, double-click to zoom in, right-click to zoom out,
// scroll wheel to zoom.
func (lv *liveView) pictureGestures() {
	p, o := lv.lens.PTZ, lv.lens.Optics
	if p.AreaZoom {
		click := gtk.NewGestureClick()
		click.SetButton(0) // all buttons
		click.ConnectPressed(func(n int, x, y float64) {
			ix, iy, ok := lv.imagePos(x, y)
			if !ok {
				return
			}
			w, h := lv.frameW, lv.frameH
			z := 100 // only center
			switch {
			case click.CurrentButton() == 3:
				z = 50
			case n == 2:
				ix, iy, z = w/2, h/2, 200 // the first click already centered
			case n > 2:
				return
			}
			lv.run(func(ctx context.Context, c *axis.Client) error { return c.PTZAreaZoom(ctx, ix, iy, w, h, z) })
		})
		lv.pic.AddController(click)
	}
	if p.RelZoom || p.AbsZoom || (o != nil && o.Zoom) {
		scroll := gtk.NewEventControllerScroll(gtk.EventControllerScrollVertical | gtk.EventControllerScrollDiscrete)
		scroll.ConnectScroll(func(_, dy float64) bool {
			if dy != 0 {
				lv.stepZoom(dy < 0)
			}
			return true
		})
		lv.pic.AddController(scroll)
	}
}

// pollOptics shows the current lens position (autofocus/calibration move the motors).
// While the user moves a slider, the values are not overwritten.
func (lv *liveView) pollOptics() {
	o := lv.lens.Optics
	if o == nil || lv.polling {
		return
	}
	lv.polling = true
	c, lens := lv.c, *o
	go func() {
		ctx, cancel := context.WithTimeout(lv.ctx, 5*time.Second)
		defer cancel()
		st, err := c.GetOptics(ctx, lens)
		glib.IdleAdd(func() {
			lv.polling = false
			if lv.ctx.Err() != nil || err != nil {
				return
			}
			switch {
			case lv.calib:
				lv.moving.SetText("Calibrating…")
			case st.Moving():
				lv.moving.SetText("Lens moving…")
			default:
				lv.moving.SetText("")
			}
			if time.Since(lv.touched) < 2*time.Second {
				return
			}
			lv.syncing = true
			switch {
			case lv.zoom != nil && lens.Legacy:
				lv.zoom.SetValue(st.ZoomPos * 100)
			case lv.zoom != nil && st.Magnification > 0:
				lv.zoom.SetValue(st.Magnification)
			}
			if lv.focus != nil {
				lv.focus.SetValue(st.Focus * 100)
			}
			lv.syncing = false
		})
	}()
}

func (lv *liveView) confirmCalibration(btn *gtk.Button) {
	o := *lv.lens.Optics
	parts := map[[2]bool]string{{true, true}: "zoom and focus", {true, false}: "zoom", {false, true}: "focus"}
	dlg := adw.NewAlertDialog("Calibrate lens?",
		fmt.Sprintf("The %s motors move through their full range to find their reference "+
			"positions. The image is blurred while this runs (up to a minute).",
			parts[[2]bool{o.CalibrateZoom, o.CalibrateFocus}]))
	af := gtk.NewCheckButtonWithLabel("Run autofocus afterwards")
	af.SetActive(o.Focus)
	if o.Focus {
		dlg.SetExtraChild(af)
	}
	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("calibrate", "Calibrate")
	dlg.SetResponseAppearance("calibrate", adw.ResponseSuggested)
	dlg.SetDefaultResponse("calibrate")
	dlg.SetCloseResponse("cancel")
	dlg.ConnectResponse(func(r string) {
		if r != "calibrate" {
			return
		}
		autofocus := o.Focus && af.Active()
		btn.SetSensitive(false)
		lv.calib = true
		lv.moving.SetText("Calibrating…")
		c := lv.c
		go func() {
			ctx, cancel := context.WithTimeout(lv.ctx, 3*time.Minute)
			defer cancel()
			err := calibrate(ctx, c, o, autofocus)
			glib.IdleAdd(func() {
				if lv.ctx.Err() != nil {
					return
				}
				lv.calib = false
				btn.SetSensitive(true)
				lv.touched = time.Time{}
				if err != nil {
					lv.moving.SetText("")
					lv.toast("Calibration: " + err.Error())
					return
				}
				lv.toast("Lens calibrated")
			})
		}()
	})
	dlg.Present(lv.dlg)
}

// calibrate runs the calibration, waits until the motors stand still and optionally
// focuses again.
func calibrate(ctx context.Context, c *axis.Client, o axis.Optics, autofocus bool) error {
	if err := c.CalibrateOptics(ctx, o); err != nil {
		return err
	}
	wait := func() error {
		time.Sleep(time.Second) // the motors may only start after the reply
		for {
			st, err := c.GetOptics(ctx, o)
			if err != nil {
				return err
			}
			if !st.Moving() {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if err := wait(); err != nil || !autofocus {
		return err
	}
	if err := c.Autofocus(ctx, o); err != nil {
		return err
	}
	return wait()
}
