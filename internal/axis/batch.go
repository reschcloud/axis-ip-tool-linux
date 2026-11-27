// SPDX-License-Identifier: AGPL-3.0-or-later

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

// ---------------------------------------------------------------- Settings

// BatchSettings are the shared settings of a batch configuration.
// Empty fields or false for Set* mean "don't change".
type BatchSettings struct {
	User, Password string
	InitPassword   bool // create the first administrator on devices in factory state
	// image settings only for devices on which the initial password was just set
	Setup InitialSetup

	SetNetwork bool
	DHCP       bool
	StartIP    string // first address of the range (static)
	Prefix     int
	Gateway    string
	DNS        []string // empty = don't change the DNS setting (with DHCP: via DHCP)

	HostnamePattern string // e.g. "camera-##" (# number, @ last IP octet); empty = don't change
	FirstNumber     int    // start value for #

	TimeZone string // empty = don't change
	SetNTP   bool
	NTP      NTPConfig

	Order string // order of IP assignment: "serial" (default), "ip", "name"
}

// Assignment is the planned target state of a device.
type Assignment struct {
	Device   Device
	IP       string // new static IPv4 ("" with DHCP or without network change)
	Hostname string // new hostname ("" = unchanged)
	User     string // this device's own, already verified credentials ("" = those from BatchSettings)
	Password string
	Problems []string
}

// Ok: no problems found.
func (a Assignment) Ok() bool { return len(a.Problems) == 0 }

// ---------------------------------------------------------------- Planung

// SortDevices puts the devices in the order of IP assignment.
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

// PlanBatch distributes IP addresses and hostnames to devs. all are all known devices
// on the network (for conflict checks); devs may be contained in it.
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
			return nil, fmt.Errorf("invalid start IP %q", s.StartIP)
		}
		if s.Prefix < 1 || s.Prefix > 30 {
			return nil, fmt.Errorf("invalid prefix /%d", s.Prefix)
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
				return nil, fmt.Errorf("address range from %s/%d is only enough for %d of %d devices",
					start, s.Prefix, i, len(as))
			}
			as[i].IP = ip.String()
			ip = ip.Next()
		}
	}

	if s.HostnamePattern != "" {
		for i := range as {
			ip := as[i].IP
			if ip == "" { // no new static address → @ from the current one
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

// ExpandHostname builds a hostname from the pattern:
//
//	#   sequential number; several # = minimum digits with leading zeros (## → 01, 02 …)
//	@   last octet of the IPv4 address (10.0.0.20 → 20)
//
// The rest is fixed text; upper case, spaces, "_" and umlauts are adjusted.
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

// ValidHostname checks a single hostname (without domain) according to RFC 1123.
func ValidHostname(h string) bool { return hostnameRe.MatchString(h) }

// CheckAssignments checks the (possibly manually edited) assignments and sets Problems.
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
				add("no IP address")
			case err != nil || !ip.Is4():
				add("invalid IP address")
			default:
				if pfx.IsValid() && !pfx.Contains(ip) {
					add("outside of %s", pfx)
				} else if pfx.IsValid() && isNetOrBroadcast(ip, pfx) {
					add("network or broadcast address")
				}
				if ip.String() == s.Gateway {
					add("is the gateway")
				}
				if ips[a.IP] > 1 {
					add("IP assigned more than once")
				}
				if o, ok := owner[a.IP]; ok && o.Key() != a.Device.Key() {
					add("IP currently belongs to %s", deviceLabel(o))
				}
			}
		}
		if a.Hostname != "" {
			if !ValidHostname(a.Hostname) {
				add("invalid hostname (a–z, 0–9, \"-\", max. 63 characters)")
			} else if hosts[a.Hostname] > 1 {
				add("hostname assigned more than once")
			}
		}
		if a.Device.NeedsSetup != nil && *a.Device.NeedsSetup && !s.InitPassword {
			add("factory state – enable \"set initial password\"")
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

// AddressInUse checks whether a device already answers at ip (TCP 80/443 reachable
// or actively refused). Without an answer the address is considered free.
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

// PlanRow is a row of a CSV plan: serial number → IP and/or hostname.
type PlanRow struct {
	Serial, IP, Hostname string
}

// ParsePlanCSV reads a plan with the columns serial number, IP, hostname.
// Separator ";", "," or tab; a header row is optional and may name the columns
// in any order (English or German names).
func ParsePlanCSV(r io.Reader) ([]PlanRow, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(string(data), "\ufeff") // BOM from Excel
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
			return nil, errors.New("CSV: column \"serial\" missing in the header row")
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
			return nil, fmt.Errorf("CSV line %d: no valid serial number %q", n+1, get(rec, "serial"))
		}
		ip, _, _ := strings.Cut(get(rec, "ip"), "/")
		rows = append(rows, PlanRow{Serial: serial, IP: ip, Hostname: strings.ToLower(get(rec, "hostname"))})
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV contains no devices")
	}
	return rows, nil
}

// ApplyPlan takes IP and hostname from rows (by serial number) and returns the serial
// numbers from rows that don't occur in as.
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

// ---------------------------------------------------------------- Execution

// BatchEvent reports the progress of a device. Step is the current step;
// Done = device finished (Err == nil: successful).
type BatchEvent struct {
	Index int
	Step  string
	Done  bool
	Err   error
}

// BatchResult is the result of a device.
type BatchResult struct {
	Assignment
	Err error
}

// RunBatch configures the devices one after another. An error on one device doesn't
// stop the others. progress is called from this goroutine.
func RunBatch(ctx context.Context, as []Assignment, s BatchSettings, progress func(BatchEvent)) []BatchResult {
	res := make([]BatchResult, len(as))
	for i, a := range as {
		res[i].Assignment = a
		if ctx.Err() != nil {
			res[i].Err = errors.New("cancelled")
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
		return errors.New("device has no reachable address")
	}
	anon := d.Client("", "")
	needs, err := anon.NeedsInitialUser(ctx)
	if errors.Is(err, ErrSetupUnknown) {
		needs, err = false, nil // cannot be determined → log in; fails with ErrNoUser if necessary
	}
	if err != nil {
		return fmt.Errorf("not reachable: %w", err)
	}
	user, pass := s.User, s.Password
	if a.User != "" && !needs {
		user, pass = a.User, a.Password
	}
	if needs {
		if !s.InitPassword {
			return errors.New("factory state – no initial password requested")
		}
		if s.User == "" || s.Password == "" {
			return errors.New("factory state – no user/password given for the initial password")
		}
		step("Initial password")
		if err := anon.CreateInitialUser(ctx, s.User, s.Password); err != nil {
			return fmt.Errorf("initial password: %w", err)
		}
	}
	if user == "" || pass == "" {
		return errors.New("no credentials for this device")
	}
	c := d.Client(user, pass)
	step("Login")
	if err := c.CheckLogin(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if needs && !s.Setup.Empty() {
		step("Power frequency/mounting")
		if _, err := c.ApplyInitialSetup(ctx, s.Setup); err != nil {
			return fmt.Errorf("power line frequency/mounting position: %w", err)
		}
	}
	if s.TimeZone != "" {
		step("Time zone")
		if err := c.SetTime(ctx, s.TimeZone, false); err != nil {
			return fmt.Errorf("time zone: %w", err)
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
		step("Network")
		cfg, err := c.GetNetwork(ctx)
		if err != nil {
			return fmt.Errorf("reading network: %w", err)
		}
		if s.SetNetwork {
			cfg.DHCP = s.DHCP
			if !s.DHCP {
				if a.IP == "" {
					return errors.New("no IP address planned")
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
			return fmt.Errorf("network: %w", err)
		}
	}
	return nil
}

// WriteResultsCSV writes a log (semicolon-separated, for spreadsheets).
func WriteResultsCSV(w io.Writer, res []BatchResult) error {
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.Write([]string{"Serial", "Model", "Old IP", "New IP", "Hostname", "Result"})
	for _, r := range res {
		result := "OK"
		if r.Err != nil {
			result = "Error: " + r.Err.Error()
		}
		cw.Write([]string{r.Device.Serial, r.Device.Model, r.Device.IP, r.IP, r.Hostname, result})
	}
	cw.Flush()
	return cw.Error()
}
