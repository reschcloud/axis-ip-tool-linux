// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"net"
	"net/url"
	"testing"
)

// The IP zone is needed for dialing but must not appear in the request URL.
func TestClientURLLeavesOutZone(t *testing.T) {
	c := NewClient("fe80::1%eth0", 80, "", "")
	if got := c.urlHost(); got != "fe80::1" {
		t.Fatalf("urlHost = %q", got)
	}
	if _, err := url.Parse("http://" + net.JoinHostPort(c.urlHost(), "80") + "/"); err != nil {
		t.Fatalf("URL with zone-less host: %v", err)
	}
	if got := NewClient("10.0.0.5", 80, "", "").urlHost(); got != "10.0.0.5" {
		t.Fatalf("urlHost = %q", got)
	}
}
