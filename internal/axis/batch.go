// SPDX-License-Identifier: GPL-3.0-or-later

package axis

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- Einstellungen

// BatchSettings sind die gemeinsamen Einstellungen einer Mehrfachkonfiguration.
// Leere Felder bzw. false bei Set* bedeuten „nicht ändern“.
type BatchSettings struct {
	User, Password string
	InitPassword   bool // auf Geräten im Werkszustand den ersten Administrator anlegen

	SetNetwork bool
	DHCP       bool
	StartIP    string // erste Adresse des Bereichs (statisch)
	Prefix     int
	Gateway    string
	DNS        []string // leer = DNS-Einstellung nicht ändern (bei DHCP: per DHCP)

	HostnamePattern string // z. B. "kamera-##" (# Nummer, @ letzte IP-Zahl); leer = nicht ändern
	FirstNumber     int    // Startwert für #

	TimeZone string // leer = nicht ändern
	SetNTP   bool
	NTP      NTPConfig

	Order string // Reihenfolge der IP-Vergabe: "serial" (Standard), "ip", "name"
}

// Assignment ist der geplante Zielzustand eines Geräts.
type Assignment struct {
	Device   Device
	IP       string // neue statische IPv4 ("" bei DHCP oder ohne Netzwerkänderung)
	Hostname string // neuer Hostname ("" = unverändert)
	User     string // eigene, bereits geprüfte Zugangsdaten dieses Geräts ("" = die aus BatchSettings)
	Password string
	Problems []string
}

// Ok: keine Probleme gefunden.
func (a Assignment) Ok() bool { return len(a.Problems) == 0 }

// ---------------------------------------------------------------- Planung

// SortDevices bringt die Geräte in die Reihenfolge der IP-Vergabe.
func SortDevices(devs []Device, order string) {
	key := func(d Device) string { return d.Serial }
	less := func(a, b string) bool { return a < b }
	switch order {
	case "ip":
		key, less = func(d Device) string { return d.IP }, IPLess
	case "name":
		key = func(d Device) string { return strings.ToLower(d.Name) }
	}
	sort.SliceStable(devs, func(i, j int) bool { return less(key(devs[i]), key(devs[j])) })
}

// PlanBatch verteilt IP-Adressen und Hostnamen auf devs. all sind alle bekannten
// Geräte im Netz (für die Konfliktprüfung); devs dürfen darin enthalten sein.
func PlanBatch(devs []Device, s BatchSettings, all []Device) ([]Assignment, error) {
	devs = append([]Device(nil), devs...)
	SortDevices(devs, s.Order)
	as := make([]Assignment, len(devs))
	for i, d := range devs {
		as[i].Device = d
	}

	if s.SetNetwork && !s.DHCP {
		start, err := netip.ParseAddr(strings.TrimSpace(s.StartIP))
		if err != nil || !start.Is4() {
			return nil, fmt.Errorf("ungültige Start-IP %q", s.StartIP)
		}
		if s.Prefix < 1 || s.Prefix > 30 {
			return nil, fmt.Errorf("ungültiges Präfix /%d", s.Prefix)
		}
		pfx := netip.PrefixFrom(start, s.Prefix).Masked()
		taken := map[string]bool{}
		mine := map[string]bool{}
		for _, d := range devs {
			mine[d.Key()] = true
		}
		for _, d := range all {
			if d.IP != "" && !mine[d.Key()] {
				taken[d.IP] = true
			}
		}
		if s.Gateway != "" {
			taken[s.Gateway] = true
		}
		ip := start
		for i := range as {
			for ip.IsValid() && pfx.Contains(ip) && (taken[ip.String()] || isNetOrBroadcast(ip, pfx)) {
				ip = ip.Next()
			}
			if !ip.IsValid() || !pfx.Contains(ip) {
				return nil, fmt.Errorf("Adressbereich ab %s/%d reicht nur für %d von %d Geräten",
					start, s.Prefix, i, len(as))
			}
			as[i].IP = ip.String()
			ip = ip.Next()
		}
	}

	if s.HostnamePattern != "" {
		for i := range as {
			ip := as[i].IP
			if ip == "" { // keine neue feste Adresse → @ aus der bisherigen
				ip = as[i].Device.IP
			}
			as[i].Hostname = ExpandHostname(s.HostnamePattern, s.FirstNumber+i, ip)
		}
	}
	CheckAssignments(as, s, all)
	return as, nil
}

func isNetOrBroadcast(ip netip.Addr, pfx netip.Prefix) bool {
	if pfx.Bits() >= 31 {
		return false
	}
	if ip == pfx.Addr() {
		return true
	}
	b := pfx.Addr().As4()
	host := uint32(1)<<(32-pfx.Bits()) - 1
	v := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) | host
	return ip == netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

var hashRe = regexp.MustCompile(`#+`)

// ExpandHostname bildet einen Hostnamen aus dem Muster:
//
//	#   fortlaufende Nummer; mehrere # = Mindeststellen mit führenden Nullen (## → 01, 02 …)
//	@   letzte Zahl der IPv4-Adresse (10.0.0.20 → 20)
//
// Der Rest ist fester Text; Großbuchstaben, Leerzeichen, „_“ und Umlaute werden angepasst.
func ExpandHostname(pattern string, nr int, ip string) string {
	s := hashRe.ReplaceAllStringFunc(pattern, func(m string) string {
		return fmt.Sprintf("%0*d", len(m), nr)
	})
	octet := ""
	if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
		octet = strconv.Itoa(int(a.As4()[3]))
	}
	s = strings.ReplaceAll(s, "@", octet)
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "-", "_", "-", "ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(s)
	return s
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHostname prüft einen einzelnen Hostnamen (ohne Domain) nach RFC 1123.
func ValidHostname(h string) bool { return hostnameRe.MatchString(h) }

// CheckAssignments prüft die (ggf. von Hand geänderten) Zuordnungen und setzt Problems.
func CheckAssignments(as []Assignment, s BatchSettings, all []Device) {
	var pfx netip.Prefix
	if start, err := netip.ParseAddr(strings.TrimSpace(s.StartIP)); err == nil && s.Prefix > 0 {
		pfx = netip.PrefixFrom(start, s.Prefix).Masked()
	}
	owner := map[string]Device{}
	for _, d := range all {
		if d.IP != "" {
			owner[d.IP] = d
		}
	}
	ips, hosts := map[string]int{}, map[string]int{}
	for _, a := range as {
		if a.IP != "" {
			ips[a.IP]++
		}
		if a.Hostname != "" {
			hosts[a.Hostname]++
		}
	}
	for i := range as {
		a := &as[i]
		a.Problems = nil
		add := func(f string, v ...any) { a.Problems = append(a.Problems, fmt.Sprintf(f, v...)) }
		if s.SetNetwork && !s.DHCP {
			ip, err := netip.ParseAddr(a.IP)
			switch {
			case a.IP == "":
				add("keine IP-Adresse")
			case err != nil || !ip.Is4():
				add("ungültige IP-Adresse")
			default:
				if pfx.IsValid() && !pfx.Contains(ip) {
					add("außerhalb von %s", pfx)
				} else if pfx.IsValid() && isNetOrBroadcast(ip, pfx) {
					add("Netz- oder Broadcast-Adresse")
				}
				if ip.String() == s.Gateway {
					add("ist das Gateway")
				}
				if ips[a.IP] > 1 {
					add("IP mehrfach vergeben")
				}
				if o, ok := owner[a.IP]; ok && o.Key() != a.Device.Key() {
					add("IP gehört derzeit %s", deviceLabel(o))
				}
			}
		}
		if a.Hostname != "" {
			if !ValidHostname(a.Hostname) {
				add("ungültiger Hostname (a–z, 0–9, „-“, max. 63 Zeichen)")
			} else if hosts[a.Hostname] > 1 {
				add("Hostname mehrfach vergeben")
			}
		}
		if a.Device.NeedsSetup != nil && *a.Device.NeedsSetup && !s.InitPassword {
			add("Werkszustand – Erstpasswort setzen aktivieren")
		}
	}
}

func deviceLabel(d Device) string {
	switch {
	case d.Name != "":
		return d.Name
	case d.Serial != "":
		return d.Serial
	}
	return d.IP
}

// AddressInUse prüft, ob unter ip schon ein Gerät antwortet (TCP 80/443 erreichbar
// oder aktiv abgelehnt). Ohne Antwort gilt die Adresse als frei.
func AddressInUse(ctx context.Context, ip string) bool {
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	used := false
	for _, port := range []string{"80", "443"} {
		wg.Add(1)
		go func(port string) {
			defer wg.Done()
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
			alive := err == nil || isRefused(err)
			if conn != nil {
				conn.Close()
			}
			if alive {
				mu.Lock()
				used = true
				mu.Unlock()
			}
		}(port)
	}
	wg.Wait()
	return used
}

func isRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// ---------------------------------------------------------------- CSV-Plan

// PlanRow ist eine Zeile eines CSV-Plans: Seriennummer → IP und/oder Hostname.
type PlanRow struct {
	Serial, IP, Hostname string
}

// ParsePlanCSV liest einen Plan mit den Spalten Seriennummer, IP, Hostname.
// Trennzeichen „;“, „,“ oder Tab; eine Kopfzeile ist optional und darf die Spalten
// in beliebiger Reihenfolge benennen.
func ParsePlanCSV(r io.Reader) ([]PlanRow, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(string(data), "\ufeff") // BOM aus Excel
	first, _, _ := strings.Cut(text, "\n")
	sep := ';'
	if strings.Count(first, "\t") > strings.Count(first, string(sep)) {
		sep = '\t'
	} else if strings.Count(first, ",") > strings.Count(first, string(sep)) {
		sep = ','
	}
	cr := csv.NewReader(strings.NewReader(text))
	cr.Comma, cr.FieldsPerRecord, cr.TrimLeadingSpace, cr.Comment = sep, -1, true, '#'
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV: %w", err)
	}
	col := map[string]int{"serial": 0, "ip": 1, "hostname": 2}
	if len(recs) > 0 && NormMAC(strings.Join(recs[0], " ")) == "" {
		col = map[string]int{"serial": -1, "ip": -1, "hostname": -1}
		for i, h := range recs[0] {
			switch strings.ToLower(strings.TrimSpace(h)) {
			case "seriennummer", "serial", "serialnumber", "sn", "mac", "mac-adresse":
				col["serial"] = i
			case "ip", "ip-adresse", "ipadresse", "adresse", "address", "ipv4":
				col["ip"] = i
			case "hostname", "host", "name":
				col["hostname"] = i
			}
		}
		if col["serial"] < 0 {
			return nil, errors.New("CSV: Spalte „Seriennummer“ fehlt in der Kopfzeile")
		}
		recs = recs[1:]
	}
	get := func(rec []string, k string) string {
		if i := col[k]; i >= 0 && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []PlanRow
	for n, rec := range recs {
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue
		}
		serial := NormMAC(get(rec, "serial"))
		if serial == "" {
			return nil, fmt.Errorf("CSV Zeile %d: keine gültige Seriennummer %q", n+1, get(rec, "serial"))
		}
		ip, _, _ := strings.Cut(get(rec, "ip"), "/")
		rows = append(rows, PlanRow{Serial: serial, IP: ip, Hostname: strings.ToLower(get(rec, "hostname"))})
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV enthält keine Geräte")
	}
	return rows, nil
}

// ApplyPlan übernimmt IP und Hostname aus rows (nach Seriennummer) und liefert die
// Seriennummern aus rows, die unter as nicht vorkommen.
func ApplyPlan(as []Assignment, rows []PlanRow) (missing []string) {
	idx := map[string]int{}
	for i, a := range as {
		idx[a.Device.Serial] = i
	}
	for _, r := range rows {
		i, ok := idx[r.Serial]
		if !ok {
			missing = append(missing, r.Serial)
			continue
		}
		if r.IP != "" {
			as[i].IP = r.IP
		}
		if r.Hostname != "" {
			as[i].Hostname = r.Hostname
		}
	}
	return missing
}

// ---------------------------------------------------------------- Ausführung

// BatchEvent meldet den Fortschritt eines Geräts. Step ist der aktuelle Schritt;
// Done = Gerät fertig (Err == nil: erfolgreich).
type BatchEvent struct {
	Index int
	Step  string
	Done  bool
	Err   error
}

// BatchResult ist das Ergebnis eines Geräts.
type BatchResult struct {
	Assignment
	Err error
}

// RunBatch konfiguriert die Geräte nacheinander. Ein Fehler bei einem Gerät hält die
// übrigen nicht auf. progress wird aus dieser Goroutine aufgerufen.
func RunBatch(ctx context.Context, as []Assignment, s BatchSettings, progress func(BatchEvent)) []BatchResult {
	res := make([]BatchResult, len(as))
	for i, a := range as {
		res[i].Assignment = a
		if ctx.Err() != nil {
			res[i].Err = errors.New("abgebrochen")
			progress(BatchEvent{Index: i, Done: true, Err: res[i].Err})
			continue
		}
		step := func(name string) { progress(BatchEvent{Index: i, Step: name}) }
		dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := configureOne(dctx, a, s, step)
		cancel()
		res[i].Err = err
		progress(BatchEvent{Index: i, Done: true, Err: err})
	}
	return res
}

func configureOne(ctx context.Context, a Assignment, s BatchSettings, step func(string)) error {
	d := a.Device
	if d.IP == "" {
		return errors.New("Gerät hat keine erreichbare Adresse")
	}
	anon := NewClient(d.IP, d.Port, "", "")
	needs, err := anon.NeedsInitialUser(ctx)
	if err != nil {
		return fmt.Errorf("nicht erreichbar: %w", err)
	}
	user, pass := s.User, s.Password
	if a.User != "" && !needs {
		user, pass = a.User, a.Password
	}
	if needs {
		if !s.InitPassword {
			return errors.New("Werkszustand – kein Erstpasswort angefordert")
		}
		if s.User == "" || s.Password == "" {
			return errors.New("Werkszustand – kein Benutzer/Passwort für das Erstpasswort angegeben")
		}
		step("Erstpasswort")
		if err := anon.CreateInitialUser(ctx, s.User, s.Password); err != nil {
			return fmt.Errorf("Erstpasswort: %w", err)
		}
	}
	if user == "" || pass == "" {
		return errors.New("keine Zugangsdaten für dieses Gerät")
	}
	c := NewClient(d.IP, d.Port, user, pass)
	step("Anmeldung")
	if err := c.CheckLogin(ctx); err != nil {
		return fmt.Errorf("Anmeldung: %w", err)
	}
	if s.TimeZone != "" {
		step("Zeitzone")
		if err := c.SetTime(ctx, s.TimeZone, false); err != nil {
			return fmt.Errorf("Zeitzone: %w", err)
		}
	}
	if s.SetNTP {
		step("NTP")
		n := s.NTP
		if cur, err := c.GetNTP(ctx); err == nil {
			n.Legacy = cur.Legacy
		}
		if err := c.SetNTP(ctx, n); err != nil {
			return fmt.Errorf("NTP: %w", err)
		}
	}
	if s.SetNetwork || a.Hostname != "" {
		step("Netzwerk")
		cfg, err := c.GetNetwork(ctx)
		if err != nil {
			return fmt.Errorf("Netzwerk lesen: %w", err)
		}
		if s.SetNetwork {
			cfg.DHCP = s.DHCP
			if !s.DHCP {
				if a.IP == "" {
					return errors.New("keine IP-Adresse geplant")
				}
				cfg.Address, cfg.Prefix, cfg.Gateway = a.IP, s.Prefix, s.Gateway
			}
			if len(s.DNS) > 0 {
				cfg.DNSFromDHCP, cfg.DNS = false, s.DNS
			} else if s.DHCP {
				cfg.DNSFromDHCP = true
			}
		}
		if a.Hostname != "" {
			cfg.HostnameFromDHCP, cfg.Hostname = false, a.Hostname
		}
		if err := c.SetNetwork(ctx, cfg); err != nil {
			return fmt.Errorf("Netzwerk: %w", err)
		}
	}
	return nil
}

// WriteResultsCSV schreibt ein Protokoll (Semikolon-getrennt, für Tabellenkalkulationen).
func WriteResultsCSV(w io.Writer, res []BatchResult) error {
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write([]string{"Seriennummer", "Modell", "Alte IP", "Neue IP", "Hostname", "Ergebnis"})
	for _, r := range res {
		result := "OK"
		if r.Err != nil {
			result = "Fehler: " + r.Err.Error()
		}
		cw.Write([]string{r.Device.Serial, r.Device.Model, r.Device.IP, r.IP, r.Hostname, result})
	}
	cw.Flush()
	return cw.Error()
}
