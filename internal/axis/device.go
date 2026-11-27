// SPDX-License-Identifier: AGPL-3.0-or-later

// Package axis contains the GUI-independent logic: discovery, VAPIX client and data model.
package axis

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Device.Source values.
const (
	SourceMDNS   = "mDNS"
	SourceSSDP   = "SSDP"
	SourceVAPIX  = "VAPIX"
	SourceManual = "Manual" // added by the user instead of discovered
)

// Device describes a discovered Axis device. Empty fields mean "unknown".
type Device struct {
	Serial          string // serial number = MAC without separators, upper case
	IP              string // address or resolved/entered hostname
	Port            int
	Scheme          string   // manual URL: "http" or "https" ("" = detect)
	HTTPSPort       int      // HTTPS port from a manual URL (0 = 443)
	ResolvedIPv4    string   // IPv4 address behind a hostname (IP stays the hostname)
	IPv6            []string // all known IPv6 addresses (without zone); display: PreferredIPv6
	IPv6Other       []string // updates only: IPv6 addresses from mDNS/DNS/manual
	IPv6Neighbor    []string // updates only: last complete neighbor-table list
	Iface           string   // network interface the device was discovered on
	Name            string
	Hostname        string // from mDNS (without ".local") or – if HostnameSet – read/set on the device
	HostnameSet     bool   // hostname comes from the device configuration (takes precedence over mDNS)
	Model           string
	Firmware        string
	NeedsSetup      *bool // nil = unknown
	Locked          *bool // device info readable only when logged in; nil = unknown
	Unlocked        bool  // valid credentials known (until AuthFailed)
	AuthFailed      bool  // the credentials used last were rejected (until Unlocked)
	Enriched        bool  // state/protocol query has finished (even if unsuccessful)
	HTTPS           *bool // device speaks HTTPS; nil = unknown
	Online          bool
	Gone            bool          // updates only: device has signed off (mDNS) → offline
	LastSeen        time.Time     // last reply from the device (ping, HTTP, mDNS, SSDP)
	Ping            time.Duration // round-trip time of the last ping, < 0 = no reply
	PingAt          time.Time     // time of the last ping (zero = never)
	Pingable        bool          // has replied to a ping at least once
	PingAddr        string        // address used for the last ping (all tried addresses without reply)
	IPv6Full        bool          // updates only: IPv6 is the complete current list
	Source          string        // "mDNS", "SSDP", "VAPIX"
	ResolveFailed   bool          // hostname could not be resolved
	ResolveKnown    bool          // ResolveFailed was determined
	RemoteIPv4      bool          // IPv4 address is outside the directly attached networks
	RemoteIPv4Known bool          // RemoteIPv4 was determined
	RemoteIPv6      bool          // preferred IPv6 address is outside the directly attached networks
	RemoteIPv6Known bool          // RemoteIPv6 was determined
}

// MaxAge: without any sign of life a device is considered offline after this time
// (Registry.Expire). Devices that don't answer pings only respond to the background check –
// maxAgeNoPing applies to them, otherwise they would briefly be "offline" between two checks.
const (
	MaxAge       = 10 * time.Second
	MaxAgeNoPing = 2*CheckInterval + 5*time.Second
	maxAgeNoPing = MaxAgeNoPing // kept for internal uses/tests
)

// Key identifies a device across discovery runs.
func (d Device) Key() string {
	if d.Serial != "" {
		return d.Serial
	}
	if d.IP == "" {
		return ""
	}
	if d.Port != 0 && d.Port != 80 {
		return net.JoinHostPort(d.IP, strconv.Itoa(d.Port))
	}
	return d.IP
}

// IPv4 returns the IPv4 address, or "" for IPv6-only devices and unresolved hostnames.
func (d Device) IPv4() string {
	if d.ResolvedIPv4 != "" {
		return d.ResolvedIPv4
	}
	a, err := netip.ParseAddr(d.IP)
	if err != nil || (a.Is6() && !a.Is4In6()) {
		return ""
	}
	return d.IP
}

// AddrFamily returns "IPv4", "IPv6" or "" for an address.
func AddrFamily(addr string) string {
	a, err := netip.ParseAddr(addr)
	switch {
	case err != nil:
		return ""
	case a.Is4() || a.Is4In6():
		return "IPv4"
	default:
		return "IPv6"
	}
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
	if u.IP != "" && !(IsAnyLinkLocal(u.IP) && d.IP != "" && !IsAnyLinkLocal(d.IP)) {
		if u.IP != d.IP {
			// Address changed (DHCP, mDNS, manual): drop metadata of the old address.
			d.ResolvedIPv4, d.Iface, d.PingAddr = "", "", ""
			d.RemoteIPv4, d.RemoteIPv4Known = false, false
			d.RemoteIPv6, d.RemoteIPv6Known = false, false
			d.ResolveFailed, d.ResolveKnown = false, false
			d.IPv6 = nil
			d.IPv6Other, d.IPv6Neighbor = nil, nil
			if a, err := netip.ParseAddr(u.IP); err == nil && a.Is6() && !a.Is4In6() && !u.IPv6Full {
				d.IPv6Other = []string{a.WithZone("").String()}
			}
		}
		d.IP = u.IP
		if u.Port != 0 {
			d.Port = u.Port
		}
	}
	set(&d.Scheme, u.Scheme)
	if u.HTTPSPort != 0 {
		d.HTTPSPort = u.HTTPSPort
	}
	set(&d.ResolvedIPv4, u.ResolvedIPv4)
	// State information only applies to the address it was determined for.
	sameAddr := u.IP == "" || u.IP == d.IP
	set(&d.Name, u.Name)
	set(&d.Iface, u.Iface)
	// IPv6 addresses come from mDNS/DNS/manual (incremental) and from the neighbor
	// table (complete list). The neighbor list must not remove mDNS/DNS addresses.
	for _, a := range d.IPv6 {
		if !slices.Contains(d.IPv6Other, a) && !slices.Contains(d.IPv6Neighbor, a) {
			d.IPv6Other = append(d.IPv6Other, a)
		}
	}
	if u.IPv6Full {
		d.IPv6Neighbor = slices.Clone(u.IPv6)
	} else {
		for _, a := range u.IPv6 {
			if !slices.Contains(d.IPv6Other, a) {
				d.IPv6Other = append(d.IPv6Other, a)
			}
		}
	}
	d.IPv6 = mergeIPv6(d.IPv6Other, d.IPv6Neighbor)
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
		set(&d.PingAddr, u.PingAddr)
	}
	d.Enriched = d.Enriched || u.Enriched
	if u.ResolveKnown {
		d.ResolveFailed, d.ResolveKnown = u.ResolveFailed, true
	}
	if u.RemoteIPv4Known {
		d.RemoteIPv4, d.RemoteIPv4Known = u.RemoteIPv4, true
	}
	if u.RemoteIPv6Known {
		d.RemoteIPv6, d.RemoteIPv6Known = u.RemoteIPv6, true
	}
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

// mergeIPv6 builds the union of address lists, preserving order.
func mergeIPv6(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, a := range l {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
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
	StatusUnresolved  = "Hostname not resolved"
	StatusNoVAPIX     = "No VAPIX response"
)

// Status returns a short, human-readable state.
func (d Device) Status() string {
	switch {
	case !d.Online:
		return StatusOffline
	case d.ResolveKnown && d.ResolveFailed:
		return StatusUnresolved
	case d.Enriched && d.NeedsSetup == nil && d.Locked == nil && !d.Unlocked && !d.AuthFailed:
		return StatusNoVAPIX
	case HTTPSOnly() && d.HTTPS != nil && !*d.HTTPS:
		return StatusNoHTTPS
	case d.NeedsSetup != nil && *d.NeedsSetup:
		return StatusNoPassword
	case d.AuthFailed:
		return StatusAuthFailed
	case d.Unlocked:
		return StatusLoggedIn
	case IsAnyLinkLocal(d.IP):
		return StatusLinkLocal
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
	scheme, host, port := "http", d.IP, d.Port
	switch {
	case d.Scheme == "https":
		scheme, port = "https", d.securePort()
	case d.Scheme == "http":
		// explicitly given by the user, keep HTTP even if HTTPS is available
	case d.HTTPS != nil && *d.HTTPS:
		scheme, port = "https", httpsPort
	}
	if port == 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	if (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		return scheme + "://" + urlHost(host) + "/"
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}

// securePort is the HTTPS port: from a manual URL or the global default.
func (d Device) securePort() int {
	if d.HTTPSPort != 0 {
		return d.HTTPSPort
	}
	return httpsPort
}

// Client creates a VAPIX client for the device, honoring a manual URL's scheme and port.
func (d Device) Client(user, pass string) *Client {
	c := NewClient(d.IP, d.Port, user, pass)
	c.scheme, c.httpsPort = d.Scheme, d.HTTPSPort
	return c
}

// urlHost encloses IPv6 addresses in brackets for use in a URL.
func urlHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
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
// interface via IPv6 – protocol as determined via IPv4 or from a manual URL.
// host == "" without a known IPv6 address.
func (d Device) WebTargetIPv6() (scheme, host string, port int) {
	host = d.IPv6Endpoint()
	switch {
	case d.Scheme == "https":
		return "https", host, d.securePort()
	case d.Scheme == "http":
		port = d.Port
		if port == 0 {
			port = 80
		}
		return "http", host, port
	case d.HTTPS != nil && *d.HTTPS:
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

// IsAnyLinkLocal: IPv4 169.254.0.0/16 or IPv6 fe80::/10.
func IsAnyLinkLocal(ip string) bool { return IsLinkLocal(ip) || IsIPv6LinkLocal(ip) }

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
