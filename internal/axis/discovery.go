package axis

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Die Suche läuft zweigleisig wie bei der AXIS IP Utility:
//   - Bonjour/mDNS über den Avahi-Dienst des Systems (D-Bus), dadurch keine
//     Konflikte mit Port 5353 und keine Firewall-Ausnahme nötig
//   - SSDP/UPnP per Multicast-M-SEARCH
// Jedes Ergebnis wird anschließend per basicdeviceinfo.cgi angereichert.

var mdnsTypes = []string{"_axis-video._tcp", "_vapix-http._tcp"}

// Discover sucht, bis ctx beendet wird. found wird aus beliebigen Goroutinen
// aufgerufen und erhält Teilinformationen, die per Device.Merge zusammenzuführen sind.
// Fehler einer Suchmethode werden über warn gemeldet, die andere läuft weiter.
func Discover(ctx context.Context, found func(Device), warn func(error)) {
	d := &discoverer{ctx: ctx, found: found, enriched: map[string]time.Time{}}
	if warn == nil {
		warn = func(error) {}
	}
	go func() {
		if err := d.avahi(); err != nil {
			warn(fmt.Errorf("Bonjour/mDNS (Avahi) nicht verfügbar: %w", err))
		}
	}()
	go func() {
		// SSDP einmal sofort und dann alle 30 s wiederholen
		for {
			if err := d.ssdp(); err != nil {
				warn(fmt.Errorf("SSDP: %w", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
}

type discoverer struct {
	ctx   context.Context
	found func(Device)

	mu       sync.Mutex
	enriched map[string]time.Time
}

func (d *discoverer) emit(dev Device) {
	d.found(dev)
	if dev.IP == "" {
		return
	}
	d.mu.Lock()
	ep := dev.Endpoint()
	if time.Since(d.enriched[ep]) < 30*time.Second {
		d.mu.Unlock()
		return
	}
	d.enriched[ep] = time.Now()
	d.mu.Unlock()
	go d.enrich(dev)
}

func (d *discoverer) enrich(dev Device) {
	ctx, cancel := context.WithTimeout(d.ctx, 6*time.Second)
	defer cancel()
	c := NewClient(dev.IP, dev.Port, "", "")
	upd := Device{Serial: dev.Serial, IP: dev.IP, Port: dev.Port, Online: true, Source: dev.Source}
	info, err := c.BasicInfo(ctx)
	if err == nil {
		upd.Model, upd.Firmware = info.Model, info.Firmware
		if info.Serial != "" {
			upd.Serial = info.Serial
		}
	}
	if err == nil || errors.Is(err, ErrAuth) {
		locked := err != nil
		upd.Locked = &locked
	}
	if needs, err := c.NeedsInitialUser(ctx); err == nil {
		upd.NeedsSetup = &needs
	}
	if ctx.Err() == nil || d.ctx.Err() == nil {
		d.found(upd)
	}
}

// ---------------------------------------------------------------- Avahi

const (
	avahiIfUnspec = int32(-1)
	avahiProtoV4  = int32(0)
)

func (d *discoverer) avahi() error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	// Signale abonnieren, *bevor* der Browser existiert – sonst gehen die ersten verloren.
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
					d.found(Device{Serial: serial, Name: label, Online: false, Source: "mDNS"})
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
		return // lokal veröffentlichte Dienste auf lo ignorieren
	}
	label, mac := splitServiceName(name)
	dev := Device{IP: addr, Port: int(port), Name: label, Online: true, Source: "mDNS"}
	for _, t := range txt {
		if k, v, ok := strings.Cut(string(t), "="); ok && strings.EqualFold(k, "macaddress") {
			dev.Serial = NormMAC(v)
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
	dev := Device{IP: ip, Port: 80, Online: true, Source: "SSDP"}
	// Nur Beschreibungen vom antwortenden Gerät selbst laden
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

// ParseSSDP liest die Header einer SSDP-Antwort (Schlüssel klein geschrieben).
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

// RootDesc enthält die interessanten Felder einer UPnP-Gerätebeschreibung.
type RootDesc struct {
	FriendlyName    string `xml:"device>friendlyName"`
	ModelName       string `xml:"device>modelName"`
	SerialNumber    string `xml:"device>serialNumber"`
	PresentationURL string `xml:"device>presentationURL"`
}

// ParseRootDesc parst rootdesc.xml (encoding/xml löst keine externen Entities auf).
func ParseRootDesc(b []byte) (RootDesc, error) {
	var r RootDesc
	err := xml.Unmarshal(b, &r)
	return r, err
}
