// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Discovery runs on two tracks like the AXIS IP Utility:
//   - Bonjour/mDNS via the system's Avahi service (D-Bus), so no conflicts with
//     port 5353 and no firewall exception needed
//   - SSDP/UPnP via multicast M-SEARCH
// Every result is then enriched via basicdeviceinfo.cgi.

var mdnsTypes = []string{"_axis-video._tcp", "_vapix-http._tcp"}

// Discovery represents a running discovery. It stops when the context passed to
// Discover is done.
type Discovery struct {
	d *discoverer
}

// Discover searches until ctx is done. found is called from arbitrary goroutines and
// receives partial information to be combined with Device.Merge.
// Errors of one discovery method are reported via warn, the other one keeps running.
// The returned Discovery accepts manual addresses that discovery didn't find.
func Discover(ctx context.Context, found func(Device), warn func(error)) *Discovery {
	d := &discoverer{ctx: ctx, found: found, enriched: map[string]time.Time{},
		enriching: map[string]bool{}, ifaces: map[string]bool{}, targets: map[string]*target{},
		enrichSem: make(chan struct{}, 16)}
	if warn == nil {
		warn = func(error) {}
	}
	go func() {
		if err := d.avahi(); err != nil {
			warn(fmt.Errorf("Bonjour/mDNS (Avahi) not available: %w", err))
		}
	}()
	go func() {
		// SSDP once right away, then repeat regularly
		for {
			if err := d.ssdp(); err != nil {
				warn(fmt.Errorf("SSDP: %w", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(ssdpInterval):
			}
		}
	}()
	go d.neighbors()
	go d.monitor(warn)
	return &Discovery{d: d}
}

// AddManual starts monitoring the entries in input exactly like discovered devices:
// ping (for IP addresses), periodic VAPIX query and expiry when they stay silent.
// input may contain several entries (see ParseManual).
func (dc *Discovery) AddManual(input string) error {
	entries, err := ParseManual(input)
	if err != nil {
		return err
	}
	for _, e := range entries {
		for _, dev := range e.Devices {
			dc.d.emit(dev)
		}
	}
	return nil
}

// AddDevice starts monitoring an already parsed manual device.
func (dc *Discovery) AddDevice(dev Device) {
	if dev.Online {
		dc.d.emit(dev)
	}
}

// CheckInterval: interval of the background checks (query, IPv6 neighbors, SSDP, login).
// Pings happen every PingInterval so that an outage is noticed quickly.
const CheckInterval = 10 * time.Second

const (
	PingInterval     = 2 * time.Second
	pingTimeout      = 600 * time.Millisecond
	enrichInterval   = CheckInterval
	neighborInterval = CheckInterval
	ssdpInterval     = CheckInterval
	resolveTTL       = time.Minute // re-resolve hostnames for the ping
)

type discoverer struct {
	ctx   context.Context
	found func(Device)

	mu        sync.Mutex
	enriched  map[string]time.Time // endpoint → last query
	enriching map[string]bool      // endpoint → query running
	ifaces    map[string]bool      // interfaces devices were found on
	targets   map[string]*target   // key (serial, otherwise IP) → monitored address
	enrichSem chan struct{}        // limits concurrent HTTP enrichment
}

// target is the address a device is monitored at (ping, query).
type target struct {
	serial    string
	ip        string // address or hostname
	port      int
	scheme    string
	httpsPort int
	online    bool
	source    string

	pingIPs       []string  // ping candidates, IPv4 first, then IPv6
	resolvedIPv4  string    // IPv4 address behind a hostname
	resolvedIPv6  []string  // IPv6 addresses behind a hostname
	iface         string    // interface for the resolved address
	resolvedAt    time.Time // last DNS lookup
	resolving     bool      // lookup running
	resolveFail   int       // consecutive failed lookups
	resolveFailed bool      // current lookup failed
	remote4       bool      // IPv4 is outside the local networks
	remote4Known  bool
	remote6       bool // preferred IPv6 is outside the local networks
	remote6Known  bool
}

func (d *discoverer) emit(dev Device) {
	d.mu.Lock()
	if dev.Iface != "" {
		d.ifaces[dev.Iface] = true
	}
	d.track(dev)
	d.mu.Unlock()
	d.found(dev)
	if dev.IP != "" {
		d.enrichSoon(dev)
	}
}

// track remembers the address to monitor (routable before link-local, like Device.Merge).
// Call with d.mu held.
func (d *discoverer) track(dev Device) {
	key := dev.Key()
	if key == "" {
		return
	}
	t := d.targets[key]
	if t == nil {
		if dev.Gone || !dev.Online {
			return
		}
		t = &target{serial: dev.Serial, source: dev.Source, scheme: dev.Scheme, httpsPort: dev.HTTPSPort}
		d.targets[key] = t
	}
	if dev.Gone {
		t.online = false
	} else if dev.Online {
		t.online = true
	}
	if dev.IP != "" && !(IsAnyLinkLocal(dev.IP) && t.ip != "" && !IsAnyLinkLocal(t.ip)) {
		if t.ip != dev.IP {
			t.pingIPs, t.resolvedIPv4, t.resolvedIPv6, t.iface = nil, "", nil, ""
			t.resolvedAt, t.resolving = time.Time{}, false
			t.resolveFail, t.resolveFailed = 0, false
			t.remote4, t.remote4Known, t.remote6, t.remote6Known = false, false, false, false
		}
		t.ip, t.port = dev.IP, dev.Port
	}
	if dev.Scheme != "" {
		t.scheme = dev.Scheme
	}
	if dev.HTTPSPort != 0 {
		t.httpsPort = dev.HTTPSPort
	}
	if dev.Serial != "" {
		t.serial = dev.Serial
	}
	if dev.RemoteIPv4Known {
		t.remote4, t.remote4Known = dev.RemoteIPv4, true
	}
	if dev.RemoteIPv6Known {
		t.remote6, t.remote6Known = dev.RemoteIPv6, true
	}
	if dev.ResolveKnown {
		t.resolveFailed = dev.ResolveFailed
	}
	if dev.Serial != "" && dev.IP != "" {
		// device was known only by address before (SSDP) → don't monitor twice
		if old := d.targets[dev.IP]; old != nil && old != t && old.serial == "" {
			delete(d.targets, dev.IP)
		}
	}
}

// enrichSoon queries dev if the last query of this address is older than enrichInterval.
func (d *discoverer) enrichSoon(dev Device) {
	ep := dev.Endpoint()
	d.mu.Lock()
	if d.enriching[ep] || time.Since(d.enriched[ep]) < enrichInterval {
		d.mu.Unlock()
		return
	}
	d.enriched[ep] = time.Now()
	d.enriching[ep] = true
	d.mu.Unlock()
	go func() {
		if d.enrichSem != nil {
			d.enrichSem <- struct{}{}
			defer func() { <-d.enrichSem }()
		}
		d.enrich(dev)
		d.mu.Lock()
		delete(d.enriching, ep)
		d.mu.Unlock()
	}()
}

// monitor pings all known devices and queries them again regularly.
func (d *discoverer) monitor(warn func(error)) {
	var warnOnce sync.Once
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(PingInterval):
		}
		d.mu.Lock()
		var ts []target
		type lookup struct{ key, host string }
		var lookups []lookup
		for key, t := range d.targets {
			if t.ip == "" {
				continue
			}
			if _, err := netip.ParseAddr(t.ip); err != nil && !t.resolving &&
				(t.resolvedAt.IsZero() || time.Since(t.resolvedAt) >= resolveTTL) {
				t.resolving = true
				lookups = append(lookups, lookup{key, t.ip})
			}
			ts = append(ts, *t)
		}
		d.mu.Unlock()
		for _, l := range lookups {
			go d.resolve(l.key, l.host)
		}
		for _, t := range ts {
			_, ipErr := netip.ParseAddr(t.ip)
			isHost := ipErr != nil
			dev := Device{Serial: t.serial, IP: t.ip, Port: t.port, Scheme: t.scheme,
				HTTPSPort: t.httpsPort, Online: true, Source: t.source,
				ResolvedIPv4: t.resolvedIPv4, IPv6: t.resolvedIPv6, Iface: t.iface,
				ResolveFailed: isHost && t.resolveFailed, ResolveKnown: isHost,
				RemoteIPv4: t.remote4, RemoteIPv4Known: t.remote4Known,
				RemoteIPv6: t.remote6, RemoteIPv6Known: t.remote6Known}
			d.enrichSoon(dev)
			cands := []string{t.ip}
			if isHost {
				cands = t.pingIPs
			}
			if len(cands) == 0 {
				// Hostname not resolved (yet): report the state without a sign of life.
				u := dev
				u.Online = false
				if d.ctx.Err() == nil {
					d.found(u)
				}
				continue
			}
			go func(t target, cands []string, isHost bool) {
				rtt, used, noReply, err := pingAny(cands, pingTimeout)
				if err != nil && !noReply {
					warnOnce.Do(func() {
						warn(fmt.Errorf("ping not possible (%v) – reachability via HTTP only", err))
					})
					return
				}
				now := time.Now()
				u := Device{Serial: t.serial, IP: t.ip, Port: t.port, Scheme: t.scheme,
					HTTPSPort: t.httpsPort, PingAt: now, Ping: rtt, PingAddr: used,
					ResolvedIPv4: t.resolvedIPv4, IPv6: t.resolvedIPv6, Iface: t.iface,
					ResolveFailed: isHost && t.resolveFailed, ResolveKnown: isHost,
					RemoteIPv4: t.remote4, RemoteIPv4Known: t.remote4Known,
					RemoteIPv6: t.remote6, RemoteIPv6Known: t.remote6Known}
				if err != nil {
					u.Ping = -1
				} else {
					u.Online, u.LastSeen = true, now
				}
				if d.ctx.Err() == nil {
					d.found(u)
				}
			}(t, cands, isHost)
		}
	}
}

// pingFn is Ping; tests replace it to simulate reachable/unreachable addresses.
var pingFn = Ping

// pingAny tries the addresses in order (IPv4 first for hostnames) and returns the
// first reply. noReply means at least one attempt timed out – then a failed fallback
// (e.g. no IPv6 route) is not reported as "ping not possible".
func pingAny(addrs []string, timeout time.Duration) (rtt time.Duration, used string, noReply bool, err error) {
	if len(addrs) == 0 {
		return 0, "", false, errors.New("no address")
	}
	var last error
	for _, a := range addrs {
		rtt, err := pingFn(a, timeout)
		if err == nil {
			return rtt, a, false, nil
		}
		last = err
		if IsTimeout(err) {
			noReply = true
		}
	}
	return 0, strings.Join(addrs, ", "), noReply, last
}

// resolve looks up a hostname and stores its addresses for ping and display. Runs in
// the background so a slow DNS server never delays the other devices.
func (d *discoverer) resolve(key, host string) {
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	var v4, v6 []string
	if err == nil {
		for _, ip := range ips {
			ip = ip.Unmap()
			switch {
			case ip.Is4():
				if s := ip.String(); !slices.Contains(v4, s) {
					v4 = append(v4, s)
				}
			case ip.Is6() && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast():
				if s := ip.String(); !slices.Contains(v6, s) {
					v6 = append(v6, s)
				}
			}
		}
	}
	var cands []string
	for i, a := range v4 {
		if i >= 2 {
			break
		}
		cands = append(cands, a)
	}
	if len(v6) > 0 {
		cands = append(cands, v6[0])
	}
	resolvedIPv4 := ""
	if len(v4) > 0 {
		resolvedIPv4 = v4[0]
	}
	primary := resolvedIPv4
	if primary == "" && len(v6) > 0 {
		primary = v6[0]
	}
	iface := ""
	if primary != "" {
		iface = IfaceForIP(primary)
	}
	remote4, remote4Known, remote6, remote6Known := false, false, false, false
	if len(v4) > 0 {
		remote4, remote4Known = IsRemoteIP(v4[0])
	}
	if len(v6) > 0 {
		remote6, remote6Known = IsRemoteIP(v6[0])
	}
	d.mu.Lock()
	if t := d.targets[key]; t != nil && t.ip == host {
		t.resolvedAt, t.resolving = time.Now(), false
		switch {
		case err != nil:
			t.resolveFail++
			t.resolveFailed = true
			if t.resolveFail >= 2 {
				// Don't keep showing/pinging an address that DNS no longer returns.
				t.pingIPs, t.resolvedIPv4, t.resolvedIPv6, t.iface = nil, "", nil, ""
				t.remote4, t.remote4Known, t.remote6, t.remote6Known = false, false, false, false
			}
		case len(cands) == 0:
			// resolved, but no usable address (e.g. only link-local)
			t.resolveFail = 0
			t.resolveFailed = true
			t.pingIPs, t.resolvedIPv4, t.resolvedIPv6, t.iface = nil, "", nil, ""
			t.remote4, t.remote4Known, t.remote6, t.remote6Known = false, false, false, false
		default:
			t.resolveFail, t.resolveFailed = 0, false
			t.pingIPs, t.resolvedIPv4, t.resolvedIPv6, t.iface = cands, resolvedIPv4, v6, iface
			t.remote4, t.remote4Known = remote4, remote4Known
			t.remote6, t.remote6Known = remote6, remote6Known
		}
	}
	d.mu.Unlock()
}

func (d *discoverer) enrich(dev Device) {
	ctx, cancel := context.WithTimeout(d.ctx, 6*time.Second)
	defer cancel()
	c := dev.Client("", "")
	defer c.hc.CloseIdleConnections() // don't leak keep-alive connections for short queries
	remote4, remote4Known := dev.RemoteIPv4, dev.RemoteIPv4Known
	if !remote4Known {
		if ip := dev.IPv4(); ip != "" {
			remote4, remote4Known = IsRemoteIP(ip)
		}
	}
	remote6, remote6Known := dev.RemoteIPv6, dev.RemoteIPv6Known
	if !remote6Known {
		if ip := dev.PreferredIPv6(); ip != "" {
			remote6, remote6Known = IsRemoteIP(ip)
		}
	}
	upd := Device{Serial: dev.Serial, IP: dev.IP, Port: dev.Port, Scheme: dev.Scheme,
		HTTPSPort: dev.HTTPSPort, Source: dev.Source, Enriched: true,
		RemoteIPv4: remote4, RemoteIPv4Known: remote4Known,
		RemoteIPv6: remote6, RemoteIPv6Known: remote6Known}
	info, err := c.BasicInfo(ctx)
	if err == nil {
		upd.Model, upd.Firmware = info.Model, info.Firmware
		if info.Serial != "" {
			upd.Serial = info.Serial
			// Let the neighbor table find IPv6 addresses for manual IP targets too.
			d.mu.Lock()
			for _, t := range d.targets {
				if t.ip == dev.IP && (dev.Port == 0 || t.port == dev.Port) {
					t.serial = info.Serial
				}
			}
			d.mu.Unlock()
		}
	}
	if err == nil || errors.Is(err, ErrAuth) {
		locked := err != nil
		upd.Locked = &locked
	}
	reachable := err == nil || errors.Is(err, ErrAuth)
	if needs, err := c.NeedsInitialUser(ctx); err == nil {
		upd.NeedsSetup = &needs
		reachable = true
	}
	// Only report the protocol if the device answered and the probe could run.
	// Otherwise leave it unknown instead of wrongly showing "HTTP".
	secure := c.Secure(ctx)
	fixed := c.scheme != ""
	known := fixed || ctx.Err() == nil
	switch {
	case fixed:
		upd.HTTPS = &secure
	case known && secure:
		upd.HTTPS = &secure
		reachable = true
	case known && reachable:
		upd.HTTPS = &secure
	case known && errors.Is(err, ErrNoHTTPS):
		no := false
		upd.HTTPS = &no
	}
	if reachable {
		upd.Online, upd.LastSeen = true, time.Now()
	}
	if ctx.Err() == nil || d.ctx.Err() == nil {
		d.found(upd)
	}
}

// ---------------------------------------------------------------- Avahi

const (
	avahiIfUnspec = int32(-1)
	avahiProtoV4  = int32(0)
	avahiProtoV6  = int32(1)
)

func (d *discoverer) avahi() error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	// Subscribe to signals *before* the browser exists – otherwise the first ones get lost.
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.Avahi.ServiceBrowser")); err != nil {
		return err
	}
	sigs := make(chan *dbus.Signal, 128)
	conn.Signal(sigs)

	server := conn.Object("org.freedesktop.Avahi", "/")
	browsers := map[dbus.ObjectPath]bool{}
	for _, t := range mdnsTypes {
		var path dbus.ObjectPath
		err := server.CallWithContext(d.ctx, "org.freedesktop.Avahi.Server.ServiceBrowserNew", 0,
			avahiIfUnspec, avahiProtoV4, t, "", uint32(0)).Store(&path)
		if err != nil {
			return err
		}
		browsers[path] = true
	}
	defer func() {
		for p := range browsers {
			conn.Object("org.freedesktop.Avahi", p).Call("org.freedesktop.Avahi.ServiceBrowser.Free", 0)
		}
	}()

	for {
		select {
		case <-d.ctx.Done():
			return nil
		case s, ok := <-sigs:
			if !ok {
				return nil
			}
			if !browsers[s.Path] || len(s.Body) < 5 {
				continue
			}
			iface, _ := s.Body[0].(int32)
			name, _ := s.Body[2].(string)
			typ, _ := s.Body[3].(string)
			domain, _ := s.Body[4].(string)
			switch s.Name {
			case "org.freedesktop.Avahi.ServiceBrowser.ItemNew":
				go d.avahiResolve(server, iface, name, typ, domain)
			case "org.freedesktop.Avahi.ServiceBrowser.ItemRemove":
				label, mac := splitServiceName(name)
				if serial := NormMAC(mac); serial != "" {
					d.emit(Device{Serial: serial, Name: label, Gone: true, Source: "mDNS"})
				}
			}
		}
	}
}

func (d *discoverer) avahiResolve(server dbus.BusObject, iface int32, name, typ, domain string) {
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	var (
		rIface, rProto, aProto int32
		rName, rType, rDomain  string
		host, addr             string
		port                   uint16
		txt                    [][]byte
		flags                  uint32
	)
	err := server.CallWithContext(ctx, "org.freedesktop.Avahi.Server.ResolveService", 0,
		iface, avahiProtoV4, name, typ, domain, avahiProtoV4, uint32(0)).
		Store(&rIface, &rProto, &rName, &rType, &rDomain, &host, &aProto, &addr, &port, &txt, &flags)
	if err != nil || addr == "" || strings.HasPrefix(addr, "127.") {
		return // ignore services published locally on lo
	}
	label, mac := splitServiceName(name)
	dev := Device{IP: addr, Port: int(port), Name: label, Online: true, Source: "mDNS", LastSeen: time.Now(),
		Hostname: strings.TrimSuffix(strings.TrimSuffix(host, "."), ".local"), Iface: IfaceName(int(rIface))}
	// IPv6 address (AAAA) if the device announces it via mDNS
	var addr6 string
	if server.CallWithContext(ctx, "org.freedesktop.Avahi.Server.ResolveService", 0,
		iface, avahiProtoV4, name, typ, domain, avahiProtoV6, uint32(0)).
		Store(&rIface, &rProto, &rName, &rType, &rDomain, &host, &aProto, &addr6, &port, &txt, &flags) == nil {
		if a, err := netip.ParseAddr(addr6); err == nil && a.Is6() && !a.IsLoopback() {
			dev.IPv6 = []string{a.WithZone("").String()}
		}
	}
	for _, t := range txt {
		// _axis-video: "macaddress=…"; _vapix-http: "sn=…" (service name there without MAC)
		if k, v, ok := strings.Cut(string(t), "="); ok && (strings.EqualFold(k, "macaddress") || strings.EqualFold(k, "sn")) {
			if mac := NormMAC(v); mac != "" {
				dev.Serial = mac
			}
		}
	}
	if dev.Serial == "" {
		dev.Serial = NormMAC(mac)
	}
	d.emit(dev)
}

// splitServiceName: "AXIS M3045-V - ACCC8E123456" -> ("AXIS M3045-V", "ACCC8E123456")
func splitServiceName(name string) (label, mac string) {
	if i := strings.LastIndex(name, " - "); i >= 0 {
		return name[:i], name[i+3:]
	}
	return name, ""
}

// ---------------------------------------------------------------- IPv6-Nachbarn

// neighbors addresses all IPv6 nodes on every interface with discovered devices and
// matches the neighbor table entries to the devices by MAC (= serial number).
func (d *discoverer) neighbors() {
	wait := 2 * time.Second // find devices (and thus interfaces) first
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = neighborInterval
		d.mu.Lock()
		var ifaces []string
		for i := range d.ifaces {
			ifaces = append(ifaces, i)
		}
		d.mu.Unlock()
		if len(ifaces) == 0 {
			wait = 2 * time.Second
			continue
		}
		for _, i := range ifaces {
			PingAllNodes(i) // errors don't matter: then just what the kernel already knows
		}
		time.Sleep(time.Second)
		nb, err := Neighbors6()
		if err != nil {
			continue
		}
		byMAC := map[string][]Neighbor6{}
		for _, n := range nb {
			byMAC[n.MAC] = append(byMAC[n.MAC], n)
		}
		d.mu.Lock()
		var upd []Device
		for _, t := range d.targets {
			if !t.online || t.serial == "" {
				continue
			}
			// complete list (even empty) so that vanished addresses are dropped
			u := Device{Serial: t.serial, IPv6Full: true, IPv6: []string{}}
			for _, n := range byMAC[t.serial] {
				u.IPv6 = append(u.IPv6, n.Addr)
				u.Iface = n.Iface
			}
			upd = append(upd, u)
		}
		d.mu.Unlock()
		for _, u := range upd {
			d.found(u)
		}
	}
}

// ---------------------------------------------------------------- SSDP

var ssdpTargets = []string{"urn:axis-com:service:BasicService:1", "upnp:rootdevice"}

func (d *discoverer) ssdp() error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for i := 0; i < 2; i++ {
		for _, st := range ssdpTargets {
			msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n" +
				"MAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
			if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
				return err
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	seen := map[string]bool{}
	buf := make([]byte, 4096)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && d.ctx.Err() == nil {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		h := ParseSSDP(string(buf[:n]))
		loc := h["location"]
		hint := strings.ToLower(h["server"] + h["st"] + h["usn"])
		if loc == "" || seen[loc] {
			continue
		}
		if !strings.Contains(hint, "axis") && !IsAxisOUI(NormMAC(h["usn"])) {
			continue
		}
		seen[loc] = true
		go d.ssdpFetch(from.IP.String(), loc)
	}
	return nil
}

func (d *discoverer) ssdpFetch(ip, location string) {
	dev := Device{IP: ip, Port: 80, Online: true, Source: "SSDP", Iface: IfaceForIPv4(ip), LastSeen: time.Now()}
	// only load descriptions from the answering device itself
	if u, err := url.Parse(location); err == nil && u.Hostname() == ip {
		ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
		if resp, err := hc.Do(req); err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
			resp.Body.Close()
			if desc, err := ParseRootDesc(body); err == nil {
				dev.Name, dev.Model = desc.FriendlyName, desc.ModelName
				dev.Serial = NormMAC(desc.SerialNumber)
				if p, err := url.Parse(desc.PresentationURL); err == nil && p.Port() != "" {
					dev.Port, _ = strconv.Atoi(p.Port())
				}
			}
		}
	}
	d.emit(dev)
}

// ParseSSDP reads the headers of an SSDP reply (keys in lower case).
func ParseSSDP(msg string) map[string]string {
	h := map[string]string{}
	lines := strings.Split(msg, "\r\n")
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok {
			h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return h
}

// RootDesc contains the interesting fields of a UPnP device description.
type RootDesc struct {
	FriendlyName    string `xml:"device>friendlyName"`
	ModelName       string `xml:"device>modelName"`
	SerialNumber    string `xml:"device>serialNumber"`
	PresentationURL string `xml:"device>presentationURL"`
}

// ParseRootDesc parses rootdesc.xml (encoding/xml doesn't resolve external entities).
func ParseRootDesc(b []byte) (RootDesc, error) {
	var r RootDesc
	err := xml.Unmarshal(b, &r)
	return r, err
}
