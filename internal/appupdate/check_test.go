// SPDX-License-Identifier: AGPL-3.0-or-later

package appupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		name, installed, body, want string
		status                      int
		wantErr                     bool
	}{
		{name: "newer numerically", installed: "0.9.2", body: `{"tag_name":"v0.10.0"}`, want: "v0.10.0"},
		{name: "newer than snapshot", installed: "0.9.2.5.gabcdef", body: `{"tag_name":"v0.9.3"}`, want: "v0.9.3"},
		{name: "same version", installed: "0.10.0", body: `{"tag_name":"v0.10.0"}`},
		{name: "same base as snapshot", installed: "0.10.0.5.gabcdef", body: `{"tag_name":"v0.10.0"}`},
		{name: "older release", installed: "0.10.0", body: `{"tag_name":"v0.9.2"}`},
		{name: "draft", installed: "0.9.2", body: `{"tag_name":"v0.10.0","draft":true}`},
		{name: "prerelease", installed: "0.9.2", body: `{"tag_name":"v0.10.0","prerelease":true}`},
		{name: "invalid tag", installed: "0.9.2", body: `{"tag_name":"v0.10.0/other"}`},
		{name: "invalid JSON", installed: "0.9.2", body: `{`, wantErr: true},
		{name: "no stable release", installed: "0.9.2", status: http.StatusNotFound},
		{name: "API failure", installed: "0.9.2", status: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/releases/latest" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			got, err := check(context.Background(), srv.Client(), srv.URL+"/releases/latest", tt.installed)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("check() = %q, %v; want %q, error: %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestCheckUnversionedBuildSkipsRequest(t *testing.T) {
	got, err := check(context.Background(), nil, ":invalid URL", "dev")
	if got != "" || err != nil {
		t.Fatalf("check() = %q, %v; want no update and no error", got, err)
	}
}
