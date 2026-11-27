// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Developer option for README screenshots: IP_TOOL_FOR_AXIS_SCREENSHOT=file.png renders the
// window into a PNG file after a few seconds and quits. GNOME doesn't let other programs
// take screenshots, and gotk4 can't pass render nodes – hence directly via the C API.

/*
#cgo pkg-config: gtk4
#include <stdlib.h>
#include <gtk/gtk.h>

static int save_widget_png(GtkWidget *w, const char *path) {
	int width = gtk_widget_get_width(w), height = gtk_widget_get_height(w);
	GdkPaintable *p = gtk_widget_paintable_new(w);
	GtkSnapshot *s = gtk_snapshot_new();
	gdk_paintable_snapshot(p, GDK_SNAPSHOT(s), width, height);
	GskRenderNode *node = gtk_snapshot_free_to_node(s);
	g_object_unref(p);
	if (node == NULL) {
		g_printerr("screenshot: no render node (%dx%d)\n", width, height);
		return 0;
	}
	GskRenderer *r = gtk_native_get_renderer(gtk_widget_get_native(w));
	if (r == NULL) {
		g_printerr("screenshot: no renderer (%dx%d)\n", width, height);
		gsk_render_node_unref(node);
		return 0;
	}
	GdkTexture *t = gsk_renderer_render_texture(r, node, NULL);
	if (t == NULL) {
		g_printerr("screenshot: no texture (%dx%d)\n", width, height);
		gsk_render_node_unref(node);
		return 0;
	}
	int ok = gdk_texture_save_to_png(t, path);
	g_object_unref(t);
	gsk_render_node_unref(node);
	return ok;
}
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"

	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
)

// screenshotAndQuit saves the window as PNG after delay seconds and quits the app.
func (w *window) screenshotAndQuit(path string, delay uint) {
	glib.TimeoutSecondsAdd(delay, func() {
		defer w.app.Quit()
		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))
		// the window content (header bar, list, action bar) – a toplevel can't paint itself
		widget := (*C.GtkWidget)(unsafe.Pointer(coreglib.InternObject(w.toasts).Native()))
		if C.save_widget_png(widget, cpath) == 0 {
			fmt.Fprintln(os.Stderr, "screenshot: could not write", path)
		}
	})
}
