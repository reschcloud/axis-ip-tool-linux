// Package axis enthält die geräteunabhängige Logik: Suche, VAPIX-Client und Datenmodell.
package axis

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Device beschreibt ein gefundenes Axis-Gerät. Leere Felder bedeuten „unbekannt“.
type Device struct {
	Serial     string // Seriennummer = MAC ohne Trennzeichen, Großbuchstaben
	IP         string
	Port       int
	Name       string
	Model      string
	Firmware   string
	NeedsSetup *bool // nil = unbekannt
	Locked     *bool // Geräteinfos nur mit Anmeldung lesbar; nil = unbekannt
	Unlocked   bool  // gültige Zugangsdaten bekannt (bleibt gesetzt)
	Online     bool
	Source     string // "mDNS", "SSDP", "VAPIX"
}

// Key identifiziert ein Gerät über Suchläufe hinweg.
func (d Device) Key() string {
	if d.Serial != "" {
		return d.Serial
	}
	return d.IP
}

// Endpoint identifiziert die Adresse eines Geräts (IP:Port), leer wenn unbekannt.
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

// Merge übernimmt alle bekannten Felder aus u.
func (d *Device) Merge(u Device) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&d.Serial, u.Serial)
	// Eine Link-Local-Adresse verdrängt keine routbare: mDNS meldet oft beide.
	if u.IP != "" && !(IsLinkLocal(u.IP) && d.IP != "" && !IsLinkLocal(d.IP)) {
		d.IP = u.IP
		if u.Port != 0 {
			d.Port = u.Port
		}
	}
	set(&d.Name, u.Name)
	set(&d.Model, u.Model)
	set(&d.Firmware, u.Firmware)
	if u.NeedsSetup != nil {
		d.NeedsSetup = u.NeedsSetup
	}
	if u.Locked != nil {
		d.Locked = u.Locked
	}
	d.Unlocked = d.Unlocked || u.Unlocked
	d.Online = u.Online
}

// Status liefert einen kurzen, menschenlesbaren Zustand.
func (d Device) Status() string {
	switch {
	case !d.Online:
		return "Offline"
	case d.NeedsSetup != nil && *d.NeedsSetup:
		return "Kein Passwort gesetzt"
	case d.IsLocked():
		return "Gesperrt"
	case IsLinkLocal(d.IP):
		return "Link-Local-Adresse"
	default:
		return "OK"
	}
}

// IsLocked: Gerät verlangt eine Anmeldung, die wir noch nicht kennen.
func (d Device) IsLocked() bool { return d.Locked != nil && *d.Locked && !d.Unlocked }

// IsLinkLocal: Adresse aus 169.254.0.0/16.
func IsLinkLocal(ip string) bool { return strings.HasPrefix(ip, "169.254.") }

var macRe = regexp.MustCompile(`(?i)([0-9a-f]{2}[:-]?){5}[0-9a-f]{2}`)

// NormMAC sucht eine MAC-Adresse in s und gibt sie als 12 Hex-Großbuchstaben zurück.
func NormMAC(s string) string {
	m := macRe.FindString(s)
	if m == "" {
		return ""
	}
	m = strings.NewReplacer(":", "", "-", "").Replace(m)
	return strings.ToUpper(m)
}

// IsAxisOUI prüft, ob die MAC zu einem Axis-Herstellerpräfix gehört.
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

// ParseMAC akzeptiert eine Seriennummer (ACCC8E123456) oder MAC (ac:cc:8e:12:34:56).
func ParseMAC(s string) (net.HardwareAddr, error) {
	n := NormMAC(s)
	if len(n) != 12 {
		return nil, fmt.Errorf("ungültige Seriennummer/MAC %q", strings.TrimSpace(s))
	}
	return net.ParseMAC(n[0:2] + ":" + n[2:4] + ":" + n[4:6] + ":" + n[6:8] + ":" + n[8:10] + ":" + n[10:12])
}

// IPLess sortiert IPv4-Adressen numerisch.
func IPLess(a, b string) bool {
	pa, pb := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if pa == nil || pb == nil {
		return a < b
	}
	return string(pa) < string(pb)
}
