// SPDX-License-Identifier: GPL-3.0-or-later

package axis

import "testing"

func TestRegistryMerge(t *testing.T) {
	r := NewRegistry()
	yes := true
	// SSDP zuerst, noch ohne Seriennummer
	if ch := r.Update(Device{IP: "10.0.0.5", Port: 80, Name: "Cam", Online: true}); !ch.Added || ch.Key != "10.0.0.5" {
		t.Fatalf("1: %+v", ch)
	}
	// Anreicherung liefert Seriennummer -> umschlüsseln
	ch := r.Update(Device{IP: "10.0.0.5", Port: 80, Serial: "ACCC8E000001", Model: "M3045", Online: true})
	if ch.Key != "ACCC8E000001" || ch.Removed != "10.0.0.5" || !ch.Added {
		t.Fatalf("2: %+v", ch)
	}
	// mDNS mit derselben Seriennummer -> gleiches Gerät
	ch = r.Update(Device{IP: "10.0.0.5", Port: 80, Serial: "ACCC8E000001", NeedsSetup: &yes, Online: true})
	if ch.Added || ch.Removed != "" || r.Len() != 1 {
		t.Fatalf("3: %+v, len %d", ch, r.Len())
	}
	d := r.Get("ACCC8E000001")
	if d.Name != "Cam" || d.Model != "M3045" || d.Status() != "Kein Passwort gesetzt" {
		t.Fatalf("Merge: %+v", d)
	}
	// Zweites Gerät, gleiche IP, anderer Port
	if ch := r.Update(Device{IP: "10.0.0.5", Port: 8080, Serial: "ACCC8E000002", Online: true}); !ch.Added {
		t.Fatalf("4: %+v", ch)
	}
	// Offline-Meldung für unbekanntes Gerät wird ignoriert
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
		t.Fatalf("IP = %s, erwartet routbare Adresse", d.IP)
	}
	// Umgekehrt: zuerst nur Link-Local bekannt, dann die echte Adresse
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
	if s := r.Get("ACCC8E000002").Status(); s != "Nicht angemeldet" {
		t.Fatalf("Status = %s", s)
	}
	r.Update(Device{Serial: "ACCC8E000002", Unlocked: true, Online: true})
	r.Update(Device{Serial: "ACCC8E000002", IP: "10.0.0.6", Online: true, Locked: &yes}) // erneute Anreicherung
	if s := r.Get("ACCC8E000002").Status(); s != "Angemeldet" {
		t.Fatalf("Status nach Entsperren = %s", s)
	}
}

func TestStateFromLinkLocalIgnored(t *testing.T) {
	r := NewRegistry()
	yes, no := true, false
	r.Update(Device{Serial: "ACCC8E000003", IP: "10.0.0.7", Online: true, HTTPS: &yes})
	r.Update(Device{Serial: "ACCC8E000003", IP: "169.254.3.3", Online: true, HTTPS: &no, Locked: &yes})
	d := r.Get("ACCC8E000003")
	if d.Connection() != "HTTPS" || d.IsLocked() {
		t.Fatalf("Zustand der Link-Local-Adresse übernommen: %s, gesperrt=%v", d.Connection(), d.IsLocked())
	}
}

func TestAuthStatus(t *testing.T) {
	r := NewRegistry()
	no := false
	ch := r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, NeedsSetup: &no, Locked: &no})
	d := r.Get(ch.Key)
	if got := d.Status(); got != "Nicht angemeldet" {
		t.Fatalf("ungeprüft: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, AuthFailed: true})
	if got := d.Status(); got != "Passwort falsch" {
		t.Fatalf("abgelehnt: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true}) // erneute Suche ändert nichts
	if got := d.Status(); got != "Passwort falsch" {
		t.Fatalf("nach Suche: %q", got)
	}
	r.Update(Device{Serial: "ACCC8E000001", IP: "10.0.0.5", Online: true, Unlocked: true})
	if got := d.Status(); got != "Angemeldet" || d.AuthFailed {
		t.Fatalf("angemeldet: %q", got)
	}
}

// Konfigurierter Hostname hat Vorrang vor dem mDNS-Namen älterer Firmware.
func TestHostnamePriority(t *testing.T) {
	r := NewRegistry()
	ch := r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "axis-accc8e000009"})
	r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "kamera-09", HostnameSet: true})
	r.Update(Device{Serial: "ACCC8E000009", IP: "10.0.0.9", Online: true, Hostname: "axis-accc8e000009"})
	if h := r.Get(ch.Key).Hostname; h != "kamera-09" {
		t.Fatalf("Hostname = %q", h)
	}
}
