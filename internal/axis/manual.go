// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Manual devices can be entered as single addresses, hostnames, complete URLs,
// CIDR prefixes or address ranges. Several entries are separated by comma, semicolon
// or whitespace.
const maxManualAddresses = 1024

// ManualEntry is one entry of the user's input (as typed) and the devices it expands to.
// A range or CIDR prefix yields several devices.
type ManualEntry struct {
	Spec    string
	Devices []Device
}

var errNoManualEntry = errors.New("no address, hostname, URL or range given")

// ParseManual parses the manual input for the camera list. Supported forms per entry:
//   - IPv4/IPv6 addresses, link-local IPv6 with interface ("fe80::1%eth0")
//   - hostnames ("camera.local", "axis-accc8e123456")
//   - complete URLs ("http://camera:8080/", "https://10.0.0.5/")
//   - CIDR prefixes ("192.168.0.0/24")
//   - ranges ("192.168.0.10-192.168.0.20", "192.168.0.10-20", "192.168.0.10..20")
//
// Entries may be separated by commas, semicolons and/or whitespace.
func ParseManual(input string) ([]ManualEntry, error) {
	specs := splitManual(input)
	if len(specs) == 0 {
		return nil, errNoManualEntry
	}
	entries := make([]ManualEntry, 0, len(specs))
	total := 0
	seen := map[string]bool{}
	for _, spec := range specs {
		if seen[spec] {
			continue
		}
		seen[spec] = true
		devs, err := manualDevices(spec)
		if err != nil {
			return nil, err
		}
		total += len(devs)
		if total > maxManualAddresses {
			return nil, fmt.Errorf("more than %d addresses – please narrow the range", maxManualAddresses)
		}
		entries = append(entries, ManualEntry{Spec: spec, Devices: devs})
	}
	return entries, nil
}

// splitManual splits on commas, semicolons and whitespace (including new lines).
// A standalone "-" or ".." between two IPs is joined again so that spaced ranges like
// "10.0.0.1 - 10.0.0.5" work; elsewhere it is ignored as a separator.
func splitManual(s string) []string {
	s = strings.NewReplacer("–", "-", "—", "-").Replace(s)
	raw := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	})
	var out []string
	for i := 0; i < len(raw); i++ {
		tok := raw[i]
		if (tok == "-" || tok == "..") && len(out) > 0 && i+1 < len(raw) {
			if _, err := netip.ParseAddr(out[len(out)-1]); err == nil && rangeEndToken(raw[i+1]) {
				out[len(out)-1] += tok + raw[i+1]
				i++
			}
			continue
		}
		out = append(out, tok)
	}
	return out
}

func rangeEndToken(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

// manualDevices parses one entry.
func manualDevices(spec string) ([]Device, error) {
	if strings.Contains(spec, "://") {
		dev, err := deviceFromURL(spec)
		if err != nil {
			return nil, err
		}
		return []Device{dev}, nil
	}
	if strings.Contains(spec, "/") {
		return devicesFromPrefix(spec)
	}
	if strings.Contains(spec, "*") {
		return devicesFromWildcard(spec)
	}
	if devs, ok, err := devicesFromRange(spec); ok {
		return devs, err
	}
	// plain IP address (including IPv6 with zone)
	if dev, err := ManualDevice(spec); err == nil {
		return []Device{dev}, nil
	} else if looksLikeIP(spec) {
		return nil, err
	}
	// hostname
	if !validHostname(spec) {
		return nil, fmt.Errorf("invalid address, hostname, URL or range %q", spec)
	}
	return []Device{manualHost(spec, "", 0)}, nil
}

// ManualDevice validates a single IP address (IPv4, IPv6, link-local IPv6 with
// "%interface") and returns the update that starts monitoring it.
func ManualDevice(addr string) (Device, error) {
	addr = strings.TrimSpace(addr)
	a, err := netip.ParseAddr(addr)
	if err != nil || a.IsUnspecified() || a.IsMulticast() {
		return Device{}, fmt.Errorf("invalid IP address %q", addr)
	}
	return deviceFromAddr(a, "", 0)
}

// deviceFromAddr builds the monitoring update for an IP address.
func deviceFromAddr(a netip.Addr, scheme string, port int) (Device, error) {
	if a.Is4In6() {
		a = a.Unmap()
	}
	if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() ||
		(a.Is4() && a == netip.AddrFrom4([4]byte{255, 255, 255, 255})) {
		return Device{}, fmt.Errorf("invalid IP address %q (not a camera address)", a)
	}
	if a.Is6() && a.IsLinkLocalUnicast() && a.Zone() == "" {
		return Device{}, fmt.Errorf("link-local IPv6 address %q needs an interface, e.g. %s%%eth0",
			a, a)
	}
	dev := manualHost(a.String(), scheme, port)
	if a.Is6() {
		dev.IPv6 = []string{a.WithZone("").String()}
	}
	dev.Iface = IfaceForIP(dev.IP)
	return dev, nil
}

// manualHost builds the monitoring update for an address or hostname.
func manualHost(host, scheme string, port int) Device {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	a, err := netip.ParseAddr(host)
	if err != nil {
		host = strings.ToLower(host) // DNS names are case-insensitive
	}
	dev := Device{IP: host, Port: 80, Online: true, Source: SourceManual, LastSeen: time.Now()}
	if err != nil {
		// A hostname is shown in the hostname column: IPv4/IPv6 columns stay empty.
		dev.Hostname = host
	} else if a.Is4() || a.Is4In6() {
		dev.RemoteIPv4, dev.RemoteIPv4Known = IsRemoteIP(host)
	} else {
		dev.RemoteIPv6, dev.RemoteIPv6Known = IsRemoteIP(host)
	}
	switch scheme {
	case "https":
		dev.Scheme = "https"
		if port != 0 && port != 443 {
			dev.HTTPSPort = port
		}
	case "http":
		dev.Scheme = "http"
		if port != 0 {
			dev.Port = port
		}
	}
	return dev
}

// deviceFromURL parses a complete URL and uses scheme, host and port.
func deviceFromURL(raw string) (Device, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Device{}, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.User != nil {
		return Device{}, fmt.Errorf("URL %q contains user name/password – use the login dialog instead", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Device{}, fmt.Errorf("only http:// and https:// URLs are supported, not %q", raw)
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" {
		return Device{}, fmt.Errorf("URL %q has no host", raw)
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Device{}, fmt.Errorf("invalid port in %q", raw)
		}
		port = n
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return deviceFromAddr(a, scheme, port)
	}
	if !validHostname(host) {
		return Device{}, fmt.Errorf("invalid host %q in URL", host)
	}
	return manualHost(host, scheme, port), nil
}

// devicesFromPrefix expands a CIDR prefix. For IPv4 network and broadcast addresses are
// skipped (except on /31 and /32).
func devicesFromPrefix(spec string) ([]Device, error) {
	p, err := netip.ParsePrefix(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid address range %q", spec)
	}
	if p.Addr().Is4In6() {
		bits := p.Bits() - 96
		if bits < 0 || bits > 32 {
			return nil, fmt.Errorf("invalid address range %q", spec)
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), bits)
	}
	if p.Addr() != p.Masked().Addr() {
		return nil, fmt.Errorf("invalid address range %q: host bits set (did you mean %s?)",
			spec, p.Masked())
	}
	p = p.Masked()
	start, end := p.Addr(), lastAddr(p)
	if start.Is4() && p.Bits() <= 30 {
		start, end = start.Next(), end.Prev()
	}
	return expandRange(start, end)
}

// lastAddr returns the highest address of the prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr()
	if a.Is4() {
		b := a.As4()
		v := binary.BigEndian.Uint32(b[:])
		if p.Bits() < 32 {
			v |= ^uint32(0) >> p.Bits()
		}
		binary.BigEndian.PutUint32(b[:], v)
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	for i := 0; i < 16; i++ {
		switch bits := p.Bits() - i*8; {
		case bits >= 8:
		case bits <= 0:
			b[i] = 0xff
		default:
			b[i] |= byte(0xff >> bits)
		}
	}
	return netip.AddrFrom16(b)
}

// devicesFromWildcard expands the common "192.168.0.*" notation (last octet only)
// into the usable host addresses .1–.254.
func devicesFromWildcard(spec string) ([]Device, error) {
	if !strings.HasSuffix(spec, ".*") || strings.Count(spec, "*") != 1 {
		return nil, fmt.Errorf("invalid address range %q (only the last octet may be \"*\")", spec)
	}
	base, err := netip.ParseAddr(strings.TrimSuffix(spec, ".*") + ".0")
	if err != nil || !base.Is4() {
		return nil, fmt.Errorf("invalid address range %q", spec)
	}
	end := base.As4()
	end[3] = 254
	return expandRange(base.Next(), netip.AddrFrom4(end))
}

// devicesFromRange parses "START-END", "START..END" and the last-octet shorthand
// "192.168.0.10-20". ok is false if spec doesn't look like a range.
func devicesFromRange(spec string) (devs []Device, ok bool, err error) {
	for _, sep := range []string{"..", "-"} {
		i := strings.Index(spec, sep)
		if i <= 0 || i+len(sep) >= len(spec) {
			continue
		}
		start, err := netip.ParseAddr(strings.TrimSpace(spec[:i]))
		if err != nil {
			if sep == ".." {
				return nil, true, fmt.Errorf("invalid address range %q", spec)
			}
			continue // e.g. a hostname with a hyphen
		}
		end, err := rangeEnd(start, strings.TrimSpace(spec[i+len(sep):]))
		if err != nil {
			return nil, true, err
		}
		if start.Is4() != end.Is4() {
			return nil, true, fmt.Errorf("range %q mixes IPv4 and IPv6", spec)
		}
		if end.Less(start) {
			return nil, true, fmt.Errorf("range %q ends before it starts", spec)
		}
		devs, err = expandRange(start, end)
		return devs, true, err
	}
	return nil, false, nil
}

// rangeEnd parses the end of a range, including the last-octet shorthand.
func rangeEnd(start netip.Addr, s string) (netip.Addr, error) {
	if n, err := strconv.Atoi(s); err == nil && start.Is4() {
		if n < 0 || n > 255 {
			return netip.Addr{}, fmt.Errorf("invalid last octet %q", s)
		}
		b := start.As4()
		if byte(n) < b[3] {
			return netip.Addr{}, fmt.Errorf("range ends before it starts")
		}
		b[3] = byte(n)
		return netip.AddrFrom4(b), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid range end %q", s)
	}
	return a, nil
}

// expandRange returns one device per address from start to end (inclusive).
func expandRange(start, end netip.Addr) ([]Device, error) {
	var out []Device
	for a := start; a.IsValid(); a = a.Next() {
		dev, err := deviceFromAddr(a, "", 0)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
		if a == end {
			return out, nil
		}
		if len(out) >= maxManualAddresses {
			return nil, fmt.Errorf("more than %d addresses – please narrow the range", maxManualAddresses)
		}
	}
	return nil, fmt.Errorf("invalid address range")
}

// looksLikeIP distinguishes a mistyped address from a hostname.
func looksLikeIP(s string) bool {
	if strings.Contains(s, ":") {
		for _, r := range s {
			if !isHex(r) && r != ':' && r != '.' && r != '%' && !(r >= '0' && r <= '9') {
				return false
			}
		}
		return true
	}
	if !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// validHostname accepts DNS/mDNS names (letters, digits, hyphen, underscore).
func validHostname(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 253 {
		return false
	}
	letter := false
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
				letter = true
			case r >= '0' && r <= '9':
			case r == '-' || r == '_':
				if r == '-' && (i == 0 || i == len(label)-1) {
					return false
				}
			default:
				return false
			}
		}
	}
	return letter
}
