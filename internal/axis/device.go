// SPDX-License-Identifier: AGPL-3.0-or-later

// Package axis contains the GUI-independent logic: discovery, VAPIX client and data model.
package axis

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Device describes a discovered Axis device. Empty fields mean "unknown".
type Device struct {
	Serial      string // serial number = MAC without separators, upper case
	IP          string
	Port        int
	IPv6        []string // all known IPv6 addresses (without zone); display: PreferredIPv6
	Iface       string   // network interface the device was discovered on
	Name        string
	Hostname    string // from mDNS (without ".local") or – if HostnameSet – read/set on the device
	HostnameSet bool   // hostname comes from the device configuration (takes precedence over mDNS)
	Model       string
	Firmware    string
	NeedsSetup  *bool // nil = unknown
	Locked      *bool // device info readable only when logged in; nil = unknown
	Unlocked    bool  // valid credentials known (until AuthFailed)
	AuthFailed  bool  // the credentials used last were rejected (until Unlocked)
	Enriched    bool  // state/protocol query has finished (even if unsuccessful)
	HTTPS       *bool // device speaks HTTPS; nil = unknown
	Online      bool
	Gone        bool          // updates only: device has signed off (mDNS) → offline
	LastSeen    time.Time     // last reply from the device (ping, HTTP, mDNS, SSDP)
	Ping        time.Duration // round-trip time of the last ping, < 0 = no reply
	PingAt      time.Time     // time of the last ping (zero = never)
	Pingable    bool          // has replied to a ping at least once
	IPv6Full    bool          // updates only: IPv6 is the complete current list
	Source      string        // "mDNS", "SSDP", "VAPIX"
}

// MaxAge: without any sign of life a device is considered offline after this time
// (Registry.Expire). Devices that don't answer pings only respond to the background check –
// maxAgeNoPing applies to them, otherwise they would briefly be "offline" between two checks.
const (
	MaxAge       = 10 * time.Second
	maxAgeNoPing = 2*CheckInterval + 5*time.Second
)

// Key identifies a device across discovery runs.
func (d Device) Key() string {
	if d.Serial != "" {
		return d.Serial
	}
	return d.IP
}

// Endpoint identifies the address of a device (IP:port), empty if unknown.
func (d Device) Endpoint() string {
	if d.IP == "" {
		return ""
	}
	p := d.Port
	if p == 0 {
		p = 80
	}
	return net.JoinHostPort(d.IP, strconv.Itoa(p))
}

// Merge takes over all known fields from u.
func (d *Device) Merge(u Device) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&d.Serial, u.Serial)
	// A link-local address doesn't replace a routable one: mDNS often reports both.
	if u.IP != "" && !(IsLinkLocal(u.IP) && d.IP != "" && !IsLinkLocal(d.IP)) {
		d.IP = u.IP
		if u.Port != 0 {
			d.Port = u.Port
		}
	}
	// State information only applies to the address it was determined for.
	sameAddr := u.IP == "" || u.IP == d.IP
	set(&d.Name, u.Name)
	set(&d.Iface, u.Iface)
	if u.IPv6Full {
		d.IPv6 = slices.Clone(u.IPv6)
	}
	for _, a := range u.IPv6 {
		if !slices.Contains(d.IPv6, a) {
			d.IPv6 = append(d.IPv6, a)
		}
	}
	if u.LastSeen.After(d.LastSeen) {
		d.LastSeen = u.LastSeen
	}
	// With older firmware mDNS announces "axis-<serial>" regardless of the configured
	// hostname – the one from the configuration therefore takes precedence.
	if u.Hostname != "" && (u.HostnameSet || !d.HostnameSet) {
		d.Hostname = u.Hostname
	}
	d.HostnameSet = d.HostnameSet || u.HostnameSet
	set(&d.Model, u.Model)
	set(&d.Firmware, u.Firmware)
	if u.NeedsSetup != nil && sameAddr {
		d.NeedsSetup = u.NeedsSetup
		if *u.NeedsSetup { // factory state: no users, so no valid login either
			d.Unlocked, d.AuthFailed = false, false
		}
	}
	if u.Locked != nil && sameAddr {
		d.Locked = u.Locked
	}
	if u.HTTPS != nil && sameAddr {
		d.HTTPS = u.HTTPS
	}
	if !u.PingAt.IsZero() && u.PingAt.After(d.PingAt) && sameAddr {
		d.Ping, d.PingAt = u.Ping, u.PingAt
		d.Pingable = d.Pingable || u.Ping >= 0
	}
	d.Enriched = d.Enriched || u.Enriched
	switch {
	case u.Unlocked:
		d.Unlocked, d.AuthFailed = true, false
		no := false // valid login → a user exists
		d.NeedsSetup = &no
	case u.AuthFailed:
		d.Unlocked, d.AuthFailed = false, true
	}
	// Online is only ever set, never cleared by missing information: devices go offline
	// by signing off (Gone) or by missing signs of life (Registry.Expire).
	switch {
	case u.Gone:
		d.Online = false
	case u.Online:
		d.Online = true
	}
}

// PingText: "0.4 ms", "no reply" or "" (not pinged yet).
func (d Device) PingText() string {
	switch {
	case d.PingAt.IsZero():
		return ""
	case d.Ping < 0:
		return "no reply"
	case d.Ping < 10*time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d.Ping)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%d ms", d.Ping.Milliseconds())
	}
}

// Status values (also compared against in the GUI).
const (
	StatusOffline     = "Offline"
	StatusNoHTTPS     = "No HTTPS"
	StatusNoPassword  = "No password set"
	StatusAuthFailed  = "Wrong password"
	StatusLinkLocal   = "Link-local address"
	StatusLoggedIn    = "Logged in"
	StatusNotLoggedIn = "Not logged in"
)

// Status returns a short, human-readable state.
func (d Device) Status() string {
	switch {
	case !d.Online:
		return StatusOffline
	case HTTPSOnly() && d.HTTPS != nil && !*d.HTTPS:
		return StatusNoHTTPS
	case d.NeedsSetup != nil && *d.NeedsSetup:
		return StatusNoPassword
	case d.AuthFailed:
		return StatusAuthFailed
	case IsLinkLocal(d.IP):
		return StatusLinkLocal
	case d.Unlocked:
		return StatusLoggedIn
	default:
		return StatusNotLoggedIn
	}
}

// Connection returns "HTTPS", "HTTP" or "" (unknown).
func (d Device) Connection() string {
	switch {
	case d.HTTPS == nil:
		return ""
	case *d.HTTPS:
		return "HTTPS"
	}
	return "HTTP"
}

// WebURL is the address of the web interface.
func (d Device) WebURL() string {
	if d.HTTPS != nil && *d.HTTPS {
		if httpsPort == 443 {
			return "https://" + d.IP + "/"
		}
		return "https://" + net.JoinHostPort(d.IP, strconv.Itoa(httpsPort)) + "/"
	}
	if d.Port != 0 && d.Port != 80 {
		return "http://" + net.JoinHostPort(d.IP, strconv.Itoa(d.Port)) + "/"
	}
	return "http://" + d.IP + "/"
}

// PreferredIPv6 is the IPv6 address to display (see PickIPv6).
func (d Device) PreferredIPv6() string { return PickIPv6(d.IPv6, d.Serial) }

// IPv6Endpoint: target for connections via IPv6, with zone for link-local ("fe80::…%eth0").
func (d Device) IPv6Endpoint() string {
	a := d.PreferredIPv6()
	if a != "" && IsIPv6LinkLocal(a) && d.Iface != "" {
		return a + "%" + d.Iface
	}
	return a
}

// WebTargetIPv6 returns scheme, host (with zone for link-local) and port of the web
// interface via IPv6 – protocol as determined via IPv4. host == "" without a known IPv6 address.
func (d Device) WebTargetIPv6() (scheme, host string, port int) {
	host = d.IPv6Endpoint()
	if d.HTTPS != nil && *d.HTTPS {
		return "https", host, httpsPort
	}
	port = d.Port
	if port == 0 {
		port = 80
	}
	return "http", host, port
}

// IsLocked: the device requires a login we don't know yet.
func (d Device) IsLocked() bool { return d.Locked != nil && *d.Locked && !d.Unlocked }

// IsLinkLocal: address from 169.254.0.0/16.
func IsLinkLocal(ip string) bool { return strings.HasPrefix(ip, "169.254.") }

var macRe = regexp.MustCompile(`(?i)([0-9a-f]{2}[:-]?){5}[0-9a-f]{2}`)

// NormMAC finds a MAC address in s and returns it as 12 upper-case hex characters.
func NormMAC(s string) string {
	m := macRe.FindString(s)
	if m == "" {
		return ""
	}
	m = strings.NewReplacer(":", "", "-", "").Replace(m)
	return strings.ToUpper(m)
}

// IsAxisOUI checks whether the MAC belongs to an Axis vendor prefix.
func IsAxisOUI(mac string) bool {
	if len(mac) < 6 {
		return false
	}
	switch mac[:6] {
	case "00408C", "ACCC8E", "B8A44F", "E82725":
		return true
	}
	return false
}

// ParseMAC accepts a serial number (ACCC8E123456) or MAC (ac:cc:8e:12:34:56).
func ParseMAC(s string) (net.HardwareAddr, error) {
	n := NormMAC(s)
	if len(n) != 12 {
		return nil, fmt.Errorf("invalid serial number/MAC %q", strings.TrimSpace(s))
	}
	return net.ParseMAC(n[0:2] + ":" + n[2:4] + ":" + n[4:6] + ":" + n[6:8] + ":" + n[8:10] + ":" + n[10:12])
}

// IPLess sorts IPv4 addresses numerically.
func IPLess(a, b string) bool {
	pa, pb := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if pa == nil || pb == nil {
		return a < b
	}
	return string(pa) < string(pb)
}
