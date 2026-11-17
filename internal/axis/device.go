// SPDX-License-Identifier: GPL-3.0-or-later

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
	Serial      string // Seriennummer = MAC ohne Trennzeichen, Großbuchstaben
	IP          string
	Port        int
	Name        string
	Hostname    string // aus mDNS (ohne „.local“) oder – wenn HostnameSet – vom Gerät gelesen/gesetzt
	HostnameSet bool   // Hostname stammt aus der Gerätekonfiguration (Vorrang vor mDNS)
	Model       string
	Firmware    string
	NeedsSetup  *bool // nil = unbekannt
	Locked      *bool // Geräteinfos nur mit Anmeldung lesbar; nil = unbekannt
	Unlocked    bool  // gültige Zugangsdaten bekannt (bis AuthFailed)
	AuthFailed  bool  // zuletzt verwendete Zugangsdaten wurden abgelehnt (bis Unlocked)
	Enriched    bool  // Abfrage von Zustand/Protokoll ist durch (auch wenn erfolglos)
	HTTPS       *bool // Gerät spricht HTTPS; nil = unbekannt
	Online      bool
	Source      string // "mDNS", "SSDP", "VAPIX"
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
	// Zustandsangaben gelten nur für die Adresse, unter der sie ermittelt wurden.
	sameAddr := u.IP == "" || u.IP == d.IP
	set(&d.Name, u.Name)
	// mDNS kündigt bei älterer Firmware „axis-<seriennummer>“ an, unabhängig vom
	// konfigurierten Hostnamen – der aus der Konfiguration hat daher Vorrang.
	if u.Hostname != "" && (u.HostnameSet || !d.HostnameSet) {
		d.Hostname = u.Hostname
	}
	d.HostnameSet = d.HostnameSet || u.HostnameSet
	set(&d.Model, u.Model)
	set(&d.Firmware, u.Firmware)
	if u.NeedsSetup != nil && sameAddr {
		d.NeedsSetup = u.NeedsSetup
		if *u.NeedsSetup { // Werkszustand: keine Benutzer, also auch keine gültige Anmeldung
			d.Unlocked, d.AuthFailed = false, false
		}
	}
	if u.Locked != nil && sameAddr {
		d.Locked = u.Locked
	}
	if u.HTTPS != nil && sameAddr {
		d.HTTPS = u.HTTPS
	}
	d.Enriched = d.Enriched || u.Enriched
	switch {
	case u.Unlocked:
		d.Unlocked, d.AuthFailed = true, false
		no := false // gültige Anmeldung → es gibt einen Benutzer
		d.NeedsSetup = &no
	case u.AuthFailed:
		d.Unlocked, d.AuthFailed = false, true
	}
	d.Online = u.Online
}

// Status liefert einen kurzen, menschenlesbaren Zustand.
func (d Device) Status() string {
	switch {
	case !d.Online:
		return "Offline"
	case HTTPSOnly() && d.HTTPS != nil && !*d.HTTPS:
		return "Kein HTTPS"
	case d.NeedsSetup != nil && *d.NeedsSetup:
		return "Kein Passwort gesetzt"
	case d.AuthFailed:
		return "Passwort falsch"
	case IsLinkLocal(d.IP):
		return "Link-Local-Adresse"
	case d.Unlocked:
		return "Angemeldet"
	default:
		return "Nicht angemeldet"
	}
}

// Connection liefert „HTTPS“, „HTTP“ oder "" (unbekannt).
func (d Device) Connection() string {
	switch {
	case d.HTTPS == nil:
		return ""
	case *d.HTTPS:
		return "HTTPS"
	}
	return "HTTP"
}

// WebURL ist die Adresse der Weboberfläche.
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
