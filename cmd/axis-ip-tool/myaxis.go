// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// My Axis sign-in in an embedded browser (WebKitGTK 6.0). The user signs in on the real
// Axis page (incl. MFA); the app only reads the session cookie axis_connect_session_sid
// of www.axis.com afterwards – it never sees the password.
//
// WebKitGTK is loaded at runtime (dlopen): no build dependency, and without the library
// the rest of the app works unchanged (the sign-in shows an install hint).

/*
#cgo pkg-config: gtk4
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>
#include <gtk/gtk.h>

typedef struct {
	int loaded;
	GtkWidget *(*view_new)(void);
	void (*load_uri)(GtkWidget *, const char *);
	const char *(*get_uri)(GtkWidget *);
	void *(*get_session)(GtkWidget *);
	void *(*default_session)(void);
	void *(*cookie_manager)(void *);
	void *(*data_manager)(void *);
	void (*get_cookies)(void *, const char *, GCancellable *, GAsyncReadyCallback, gpointer);
	GList *(*get_cookies_finish)(void *, GAsyncResult *, GError **);
	void (*clear)(void *, int, GTimeSpan, GCancellable *, GAsyncReadyCallback, gpointer);
	const char *(*cookie_name)(void *);
	const char *(*cookie_value)(void *);
	GDateTime *(*cookie_expires)(void *);
	void (*cookie_free)(void *);
} wk_api;

static wk_api wk;

static int wk_load(void) {
	if (wk.loaded)
		return wk.loaded > 0;
	wk.loaded = -1;
	void *w = dlopen("libwebkitgtk-6.0.so.4", RTLD_NOW | RTLD_GLOBAL);
	void *s = dlopen("libsoup-3.0.so.0", RTLD_NOW | RTLD_GLOBAL);
	if (!w || !s)
		return 0;
#define SYM(lib, field, name) if (!(*(void **)&wk.field = dlsym(lib, name))) return 0;
	SYM(w, view_new, "webkit_web_view_new")
	SYM(w, load_uri, "webkit_web_view_load_uri")
	SYM(w, get_uri, "webkit_web_view_get_uri")
	SYM(w, get_session, "webkit_web_view_get_network_session")
	SYM(w, default_session, "webkit_network_session_get_default")
	SYM(w, cookie_manager, "webkit_network_session_get_cookie_manager")
	SYM(w, data_manager, "webkit_network_session_get_website_data_manager")
	SYM(w, get_cookies, "webkit_cookie_manager_get_cookies")
	SYM(w, get_cookies_finish, "webkit_cookie_manager_get_cookies_finish")
	SYM(w, clear, "webkit_website_data_manager_clear")
	SYM(s, cookie_name, "soup_cookie_get_name")
	SYM(s, cookie_value, "soup_cookie_get_value")
	SYM(s, cookie_expires, "soup_cookie_get_expires")
	SYM(s, cookie_free, "soup_cookie_free")
#undef SYM
	wk.loaded = 1;
	return 1;
}

// result of the last cookie query: 0 none, 1 running, 2 done
static int ck_state;
static char *ck_value;
static gint64 ck_expires;

static void ck_done(GObject *mgr, GAsyncResult *res, gpointer data) {
	GList *list = wk.get_cookies_finish(mgr, res, NULL);
	g_free(ck_value);
	ck_value = NULL;
	ck_expires = 0;
	for (GList *l = list; l; l = l->next) {
		if (!ck_value && strcmp(wk.cookie_name(l->data), (const char *)data) == 0) {
			ck_value = g_strdup(wk.cookie_value(l->data));
			GDateTime *e = wk.cookie_expires(l->data);
			if (e)
				ck_expires = g_date_time_to_unix(e);
		}
		wk.cookie_free(l->data);
	}
	g_list_free(list);
	ck_state = 2;
}

static void wk_query_cookie(GtkWidget *view, const char *uri, const char *name) {
	if (ck_state == 1)
		return;
	ck_state = 1;
	void *mgr = wk.cookie_manager(wk.get_session(view));
	wk.get_cookies(mgr, uri, NULL, ck_done, (gpointer)name);
}

static GtkWidget *wk_new_view(GtkBox *box, const char *uri) {
	GtkWidget *v = wk.view_new();
	gtk_widget_set_hexpand(v, TRUE);
	gtk_widget_set_vexpand(v, TRUE);
	gtk_box_append(box, v);
	wk.load_uri(v, uri);
	return v;
}

static const char *wk_get_uri(GtkWidget *v) { return wk.get_uri(v); }

// ck_take returns the finished query (1) and hands over value/expiry; 0 = not finished.
static int ck_take(char **value, gint64 *expires) {
	if (ck_state != 2)
		return 0;
	ck_state = 0;
	*value = ck_value;
	*expires = ck_expires;
	ck_value = NULL;
	return 1;
}

// forget the cookies of the embedded browser (WEBKIT_WEBSITE_DATA_COOKIES) – full sign-out
static void wk_clear_cookies(void) {
	wk.clear(wk.data_manager(wk.default_session()), 1 << 8, 0, NULL, NULL, NULL);
}
*/
import "C"

import (
	"context"
	"net/url"
	"strings"
	"time"
	"unsafe"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"axis-ip-tool/internal/axis"
)

// cookieName must stay alive while a query runs (passed to C as user data).
var cookieName = C.CString(axis.MyAxisCookie)

// webkitAvailable reports whether WebKitGTK 6.0 could be loaded.
func webkitAvailable() bool { return C.wk_load() != 0 }

const webkitHint = "The sign-in needs WebKitGTK 6.0 (Fedora: webkitgtk6.0, " +
	"Debian/Ubuntu: libwebkitgtk-6.0-4, Arch: webkitgtk-6.0)."

// myAxisSignOut forgets the stored session and the data of the embedded browser.
func myAxisSignOut() error {
	if webkitAvailable() {
		C.wk_clear_cookies()
	}
	return axis.ForgetMyAxisSession()
}

// myAxisLogin shows the Axis sign-in page. done is called (GTK thread) with the
// verified session; it is not called if the dialog is closed before.
func myAxisLogin(parent gtk.Widgetter, cat *axis.FirmwareCatalog, done func(axis.MyAxisSession)) bool {
	if !webkitAvailable() {
		return false
	}
	dlg := adw.NewDialog()
	dlg.SetTitle("Sign in to My Axis")
	dlg.SetContentWidth(960)
	dlg.SetContentHeight(760)

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	status := gtk.NewLabel("Sign in with your Axis account. The app only keeps the session " +
		"for firmware downloads, never your password.")
	status.AddCSSClass("dim-label")
	status.SetWrap(true)
	bar := gtk.NewActionBar()
	bar.SetCenterWidget(status)
	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(box)
	tv.AddBottomBar(bar)
	dlg.SetChild(tv)

	curl := C.CString(axis.MyAxisLoginURL)
	view := C.wk_new_view((*C.GtkBox)(unsafe.Pointer(coreglib.InternObject(box).Native())), curl)
	C.free(unsafe.Pointer(curl))
	site := C.CString("https://www.axis.com/")

	ctx, cancel := context.WithCancel(context.Background())
	checking := false
	var lastTried string
	dlg.ConnectClosed(func() {
		cancel()
		C.free(unsafe.Pointer(site))
	})
	glib.TimeoutAdd(700, func() bool {
		if ctx.Err() != nil {
			return false
		}
		var cval *C.char
		var cexp C.gint64
		if C.ck_take(&cval, &cexp) != 0 {
			if cval == nil {
				return true
			}
			value := C.GoString(cval)
			C.g_free(C.gpointer(cval))
			if checking {
				return true
			}
			var exp time.Time
			if cexp > 0 {
				exp = time.Unix(int64(cexp), 0)
			}
			if value == lastTried {
				return true // already rejected by axis.com
			}
			checking = true
			status.SetText("Checking the sign-in …")
			go func() {
				cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
				err := cat.CheckMyAxisSession(cctx, value)
				ccancel()
				glib.IdleAdd(func() {
					checking = false
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						lastTried = value
						status.SetText("Not signed in yet – " + err.Error())
						return
					}
					s := axis.MyAxisSession{Value: value, Expires: exp}
					if err := axis.SaveMyAxisSession(s); err != nil {
						status.SetText("Session could not be saved: " + err.Error())
					}
					dlg.Close()
					done(s)
				})
			}()
			return true
		}
		// ask for the cookie once a page of www.axis.com outside the login has loaded
		if u := C.GoString(C.wk_get_uri(view)); u != "" && !checking {
			if pu, err := url.Parse(u); err == nil && pu.Host == "www.axis.com" &&
				!strings.Contains(pu.Path, "/my-axis/login") {
				C.wk_query_cookie(view, site, cookieName)
			}
		}
		return true
	})
	dlg.Present(parent)
	return true
}
