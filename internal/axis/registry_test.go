// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"testing"
	"time"
)

func TestRegistryMerge(t *testing.T) {
	r := NewRegistry()
	yes := true
	// SSDP first, still without serial number
	if ch := r.Update(Device{IP: "10.0.0.5", Port: 80, Name: "Cam", Online: true}); !ch.Added || ch.Key != "10.0.0.5" {
		t.Fatalf("1: %+v", ch)
	}
	// enrichment delivers the serial number -> re-key
	ch := r.Update(Device{IP: "10.0.0.5", Port: 80, Serial: "ACCC8E000001", Model: "M3045", Online: true})
	if ch.Key != "ACCC8E000001" || ch.Removed != "10.0.0.5" || !ch.Added {
		t.Fatalf("2: %+v", ch)
	}
	// mDNS with the same serial number -> same device
	ch = r.Update(Device{IP: "10.0.0.5", Port: 80, Serial: "ACCC8E000001", NeedsSetup: &yes, Online: true})
	if ch.Added || ch.Removed != "" || r.Len() != 1 {
		t.Fatalf("3: %+v, len %d", ch, r.Len())
	}
	d := r.Get("ACCC8E000001")
	if d.Name != "Cam" || d.Model != "M3045" || d.Status() != StatusNoPassword {
		t.Fatalf("Merge: %+v", d)
	}
	// second device, same IP, different port
	if ch := r.Update(Device{IP: "10.0.0.5", Port: 8080, Serial: "ACCC8E000002", Online: true}); !ch.Added {
		t.Fatalf("4: %+v", ch)
	}
	// offline message for an unknown device is ignored
	if ch := r.Update(Device{Serial: "ACCC8E000009", Online: false}); ch.Key != "" {
		t.Fatalf("5: %+v", ch)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d", r.Len())
	}
}

func TestLinkLocalDoesNotReplaceRoutable(t *testing.T) {
	r := NewRegistry()
	r.Update(Device{Serial: "ACCC8EBD0790", IP: "100.64.19.186", Port: 80, Online: true})
	r.Update(Device{Serial: "ACCC8EBD0790", IP: "169.254.12.34", Port: 80, Online: true})
	if d := r.Get("ACCC8EBD0790"); d.IP != "100.64.19.186" {
		t.Fatalf("IP = %s, expected routable address", d.IP)
	}
	// the other way round: only link-local known first, then the real address
	r.Update(Device{Serial: "ACCC8E000001", IP: "169.254.1.1", Online: true})
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true})
	if d := r.Get("ACCC8E000001"); d.IP != "10.0.0.5" {
		t.Fatalf("IP = %s", d.IP)
	}
}

func TestLockedStatus(t *testing.T) {
	r := NewRegistry()
	yes := true
	r.Update(Device{Serial: "ACCC8E000002", IP: "10.0.0.6", Online: true, Locked: &yes})
	if s := r.Get("ACCC8E000002").Status(); s != StatusNotLoggedIn {
		t.Fatalf("Status = %s", s)
	}
	r.Update(Device{Serial: "ACCC8E000002", Unlocked: true, Online: true})
	r.Update(Device{Serial: "ACCC8E000002", IP: "10.0.0.6", Online: true, Locked: &yes}) // erneute Anreicherung
	if s := r.Get("ACCC8E000002").Status(); s != StatusLoggedIn {
		t.Fatalf("status after unlocking = %s", s)
	}
}

func TestStateFromLinkLocalIgnored(t *testing.T) {
	r := NewRegistry()
	yes, no := true, false
	r.Update(Device{Serial: "ACCC8E000003", IP: "10.0.0.7", Online: true, HTTPS: &yes})
	r.Update(Device{Serial: "ACCC8E000003", IP: "169.254.3.3", Online: true, HTTPS: &no, Locked: &yes})
	d := r.Get("ACCC8E000003")
	if d.Connection() != "HTTPS" || d.IsLocked() {
		t.Fatalf("state of the link-local address applied: %s, locked=%v", d.Connection(), d.IsLocked())
	}
}

func TestAuthStatus(t *testing.T) {
	r := NewRegistry()
	no := false
	ch := r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, NeedsSetup: &no, Locked: &no})
	d := r.Get(ch.Key)
	if got := d.Status(); got != StatusNotLoggedIn {
		t.Fatalf("unverified: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, AuthFailed: true})
	if got := d.Status(); got != StatusAuthFailed {
		t.Fatalf("abgelehnt: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true}) // another discovery run changes nothing
	if got := d.Status(); got != StatusAuthFailed {
		t.Fatalf("after discovery: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, Unlocked: true})
	if got := d.Status(); got != StatusLoggedIn || d.AuthFailed {
		t.Fatalf("logged in: %q", got)
	}
}

// The configured hostname takes precedence over the mDNS name of older firmware.
func TestHostnamePriority(t *testing.T) {
	r := NewRegistry()
	ch := r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "axis-accc8e000009"})
	r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "camera-09", HostnameSet: true})
	r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "axis-accc8e000009"})
	if h := r.Get(ch.Key).Hostname; h != "camera-09" {
		t.Fatalf("Hostname = %q", h)
	}
}

// Devices without a sign of life go offline after MaxAge; a ping brings them back.
func TestRegistryExpire(t *testing.T) {
	r := NewRegistry()
	t0 := time.Now()
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, LastSeen: t0,
		PingAt: t0, Ping: time.Millisecond}) // answers pings → short limit
	// updates without online information (e.g. failed ping) don't set offline
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", PingAt: t0.Add(time.Second), Ping: -1})
	d := r.Get("ACCC8E000001")
	if !d.Online || d.PingText() != "no reply" {
		t.Fatalf("after failed ping: Online=%v Ping=%q", d.Online, d.PingText())
	}
	if got := r.Expire(t0.Add(MaxAge), MaxAge); len(got) != 0 {
		t.Fatalf("expired too early: %v", got)
	}
	if got := r.Expire(t0.Add(MaxAge+time.Second), MaxAge); len(got) != 1 || d.Online || d.Status() != StatusOffline {
		t.Fatalf("Expire = %v, Online=%v", got, d.Online)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, LastSeen: t0.Add(12 * time.Second),
		PingAt: t0.Add(12 * time.Second), Ping: 1500 * time.Microsecond})
	if !d.Online || d.PingText() != "1.5 ms" {
		t.Fatalf("after ping: Online=%v Ping=%q", d.Online, d.PingText())
	}
	// Abmeldung per mDNS
	r.Update(Device{Serial: "ACCC8E000001", Gone: true})
	if d.Online {
		t.Fatal("Gone ignoriert")
	}
}

// The neighbor table delivers the complete list: vanished addresses are dropped.
func TestIPv6Full(t *testing.T) {
	d := Device{Serial: "ACCC8E000001", IPv6: []string{"fe80::1", "2001:db8::1"}}
	d.Merge(Device{Serial: "ACCC8E000001", IPv6Full: true, IPv6: []string{"fe80::1"}})
	if len(d.IPv6) != 1 {
		t.Fatalf("IPv6 = %v", d.IPv6)
	}
	d.Merge(Device{Serial: "ACCC8E000001", IPv6Full: true, IPv6: []string{}})
	if len(d.IPv6) != 0 {
		t.Fatalf("IPv6 = %v", d.IPv6)
	}
}

// Devices without ping replies only report every CheckInterval – not "offline" in between.
func TestExpireWithoutPing(t *testing.T) {
	r := NewRegistry()
	t0 := time.Now()
	r.Update(Device{Serial: "ACCC8E000002", IP: "10.0.0.6", Online: true, LastSeen: t0, PingAt: t0, Ping: -1})
	if got := r.Expire(t0.Add(CheckInterval+3*time.Second), MaxAge); len(got) != 0 {
		t.Fatalf("device without ping offline too early: %v", got)
	}
	if got := r.Expire(t0.Add(maxAgeNoPing+time.Second), MaxAge); len(got) != 1 {
		t.Fatalf("device without ping never offline: %v", got)
	}
}
