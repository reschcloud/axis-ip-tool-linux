// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// Hostnames are resolved in the background for ping, IPv4/IPv6 display and interface.
func TestResolveForPing(t *testing.T) {
	d := &discoverer{ctx: context.Background(), targets: map[string]*target{
		"localhost": {ip: "localhost"},
	}}
	d.resolve("localhost", "localhost")
	tg := d.targets["localhost"]
	if tg.resolving {
		t.Fatal("resolve flag still set")
	}
	if tg.resolvedAt.IsZero() {
		t.Fatal("lookup did not finish")
	}
	if tg.resolvedIPv4 == "" {
		t.Skip("localhost does not resolve in this environment")
	}
	if _, err := netip.ParseAddr(tg.resolvedIPv4); err != nil {
		t.Fatalf("resolvedIPv4 = %q", tg.resolvedIPv4)
	}
	if !slices.Contains(tg.pingIPs, tg.resolvedIPv4) || tg.pingIPs[0] != tg.resolvedIPv4 {
		t.Fatalf("pingIPs = %v, IPv4 = %q", tg.pingIPs, tg.resolvedIPv4)
	}
	if tg.iface == "" {
		t.Fatal("interface not determined")
	}
	if time.Since(tg.resolvedAt) > time.Minute {
		t.Fatal("resolvedAt is not current")
	}
}

type testTimeout struct{}

func (testTimeout) Error() string   { return "timeout" }
func (testTimeout) Timeout() bool   { return true }
func (testTimeout) Temporary() bool { return true }

// IPv4 is tried first; on timeout the next candidate (IPv6) is used.
func TestPingFallback(t *testing.T) {
	old := pingFn
	t.Cleanup(func() { pingFn = old })

	tests := []struct {
		name    string
		cands   []string
		ok      map[string]bool // addresses that reply
		used    string
		noReply bool
		err     bool
	}{
		{"v4 replies", []string{"10.0.0.1", "2001:db8::1"}, map[string]bool{"10.0.0.1": true},
			"10.0.0.1", false, false},
		{"v4 timeout, v6 replies", []string{"10.0.0.1", "2001:db8::1"}, map[string]bool{"2001:db8::1": true},
			"2001:db8::1", false, false},
		{"all timeout", []string{"10.0.0.1", "2001:db8::1"}, nil,
			"10.0.0.1, 2001:db8::1", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pingFn = func(addr string, _ time.Duration) (time.Duration, error) {
				if tt.ok[addr] {
					return 5 * time.Millisecond, nil
				}
				return 0, testTimeout{}
			}
			_, used, noReply, err := pingAny(tt.cands, 100*time.Millisecond)
			if used != tt.used || noReply != tt.noReply || (err != nil) != tt.err {
				t.Fatalf("used=%q noReply=%v err=%v", used, noReply, err)
			}
		})
	}
}
