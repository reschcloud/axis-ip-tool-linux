// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"errors"
	"testing"
)

func TestChangePassword(t *testing.T) {
	_, host, port := startFake(t)
	ctx := context.Background()
	if err := NewClient(host, port, "", "").CreateInitialUser(ctx, "root", "geheim123"); err != nil {
		t.Fatal(err)
	}
	c := NewClient(host, port, "root", "geheim123")
	if err := c.CheckLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ChangePassword(ctx, "root", "neuespasswort"); err != nil {
		t.Fatal(err)
	}
	if err := NewClient(host, port, "root", "geheim123").CheckLogin(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("old password still valid: %v", err)
	}
	if err := NewClient(host, port, "root", "neuespasswort").CheckLogin(ctx); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}
