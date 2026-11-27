// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"slices"
	"testing"
)

func TestManualDevice(t *testing.T) {
	tests := []struct {
		addr    string
		wantIP  string
		wantV6  []string
		iface   string
		wantErr bool
	}{
		{addr: "10.0.0.5", wantIP: "10.0.0.5"},
		{addr: " 10.0.0.5 ", wantIP: "10.0.0.5"},
		{addr: "::ffff:10.0.0.5", wantIP: "10.0.0.5"}, // IPv4-mapped address
		{addr: "2001:db8::1", wantIP: "2001:db8::1", wantV6: []string{"2001:db8::1"}},
		{addr: "fe80::1%eth0", wantIP: "fe80::1%eth0", wantV6: []string{"fe80::1"}, iface: "eth0"},
		{addr: "fe80::1", wantErr: true}, // link-local needs an interface
		{addr: "not-an-ip", wantErr: true},
		{addr: "ff02::1", wantErr: true}, // multicast
		{addr: "::", wantErr: true},      // unspecified
		{addr: "", wantErr: true},
	}
	for _, tt := range tests {
		dev, err := ManualDevice(tt.addr)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", tt.addr, dev)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tt.addr, err)
			continue
		}
		if dev.IP != tt.wantIP || !dev.Online || dev.Source != SourceManual || dev.Port != 80 ||
			dev.LastSeen.IsZero() {
			t.Errorf("%q: %+v", tt.addr, dev)
		}
		if !slices.Equal(dev.IPv6, tt.wantV6) {
			t.Errorf("%q: IPv6 = %v, expected %v", tt.addr, dev.IPv6, tt.wantV6)
		}
		if dev.Iface != tt.iface && tt.iface != "" {
			t.Errorf("%q: Iface = %q, expected %q", tt.addr, dev.Iface, tt.iface)
		}
	}
}

// An IPv6-only device uses its address as primary address, but not in the IPv4 column.
func TestIPv4AndWebURL(t *testing.T) {
	if got := (Device{IP: "10.0.0.5"}).IPv4(); got != "10.0.0.5" {
		t.Fatalf("IPv4 = %q", got)
	}
	if got := (Device{IP: "2001:db8::1"}).IPv4(); got != "" {
		t.Fatalf("IPv4 = %q", got)
	}
	if got := (Device{IP: "cam.local", ResolvedIPv4: "10.0.0.5"}).IPv4(); got != "10.0.0.5" {
		t.Fatalf("resolved IPv4 = %q", got)
	}
	if got := AddrFamily("10.0.0.5"); got != "IPv4" {
		t.Fatalf("family = %q", got)
	}
	if got := AddrFamily("fe80::1%eth0"); got != "IPv6" {
		t.Fatalf("family = %q", got)
	}
	if got := AddrFamily("camera.local"); got != "" {
		t.Fatalf("family = %q", got)
	}
	if got := (Device{IP: "2001:db8::1"}).WebURL(); got != "http://[2001:db8::1]/" {
		t.Fatalf("WebURL = %q", got)
	}
	if got := (Device{IP: "10.0.0.5", Port: 8080}).WebURL(); got != "http://10.0.0.5:8080/" {
		t.Fatalf("WebURL = %q", got)
	}
	yes := true
	if got := (Device{IP: "2001:db8::1", HTTPS: &yes}).WebURL(); got != "https://[2001:db8::1]/" {
		t.Fatalf("WebURL = %q", got)
	}
}

func TestIPv6LinkLocalStatus(t *testing.T) {
	if got := (Device{IP: "fe80::1", Online: true}).Status(); got != StatusLinkLocal {
		t.Fatalf("Status = %q", got)
	}
	// a valid login is more informative than the link-local hint
	if got := (Device{IP: "fe80::1", Online: true, Unlocked: true}).Status(); got != StatusLoggedIn {
		t.Fatalf("Status = %q", got)
	}
}

func TestNewStatuses(t *testing.T) {
	if got := (Device{Online: true, ResolveKnown: true, ResolveFailed: true}).Status(); got != StatusUnresolved {
		t.Fatalf("Status = %q", got)
	}
	// reachable, but no VAPIX information at all
	if got := (Device{Online: true, Enriched: true, IP: "10.0.0.5"}).Status(); got != StatusNoVAPIX {
		t.Fatalf("Status = %q", got)
	}
	yes, no := true, false
	if got := (Device{Online: true, Enriched: true, IP: "10.0.0.5", NeedsSetup: &no, Locked: &yes}).Status(); got == StatusNoVAPIX {
		t.Fatalf("Axis device classified as NoVAPIX: %q", got)
	}
}

func TestKeyIncludesNonDefaultPort(t *testing.T) {
	if got := (Device{IP: "camera.local", Port: 8080}).Key(); got != "camera.local:8080" {
		t.Fatalf("Key = %q", got)
	}
	if got := (Device{IP: "10.0.0.5", Port: 80}).Key(); got != "10.0.0.5" {
		t.Fatalf("Key = %q", got)
	}
}

// Address changes must not keep metadata of the old address.
func TestMergeClearsAddressMetadata(t *testing.T) {
	d := Device{IP: "10.0.0.5", Iface: "eth0", ResolvedIPv4: "10.0.0.5",
		IPv6: []string{"2001:db8::5"}, RemoteIPv4: true, RemoteIPv4Known: true, PingAddr: "10.0.0.5"}
	d.Merge(Device{IP: "10.0.0.6"})
	if d.IP != "10.0.0.6" || d.Iface != "" || d.ResolvedIPv4 != "" || len(d.IPv6) != 0 ||
		d.RemoteIPv4Known || d.PingAddr != "" {
		t.Fatalf("stale metadata after address change: %+v", d)
	}
}
