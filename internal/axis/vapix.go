package axis

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrAuth: Zugangsdaten fehlen, sind falsch oder haben keine Admin-Rechte.
var ErrAuth = errors.New("Benutzername oder Passwort falsch")

// ErrNotSupported: Das Gerät kennt die (neue) API nicht.
var ErrNotSupported = errors.New("von der Firmware nicht unterstützt")

// Client spricht VAPIX mit einem einzelnen Gerät.
type Client struct {
	base       string
	user, pass string
	hc         *http.Client

	mu   sync.Mutex
	chal *challenge
}

// NewClient erzeugt einen Client. user darf leer sein (nur offene Endpunkte).
func NewClient(host string, port int, user, pass string) *Client {
	if port == 0 {
		port = 80
	}
	return &Client{
		base: "http://" + net.JoinHostPort(host, strconv.Itoa(port)),
		user: user, pass: pass,
		hc: &http.Client{
			Timeout: 8 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil, // Kameras sind lokal, nie über einen Proxy
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, ctype string, auth bool) (int, []byte, error) {
	send := func(authz string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if authz == "basic" {
			req.SetBasicAuth(c.user, c.pass)
		} else if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		return c.hc.Do(req)
	}
	authFor := func(ch *challenge) string {
		if ch == nil {
			return ""
		}
		if ch.scheme == "basic" {
			return "basic"
		}
		return ch.authorization(method, path, c.user, c.pass)
	}

	var authz string
	if auth && c.user != "" {
		c.mu.Lock()
		authz = authFor(c.chal)
		c.mu.Unlock()
	}
	resp, err := send(authz)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && auth && c.user != "" {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ch := parseChallenges(resp.Header.Values("WWW-Authenticate"))
		if ch == nil {
			return 0, nil, ErrAuth
		}
		c.mu.Lock()
		c.chal = ch
		authz = authFor(ch)
		c.mu.Unlock()
		if resp, err = send(authz); err != nil {
			return 0, nil, err
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, err
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return resp.StatusCode, data, ErrAuth
	case http.StatusForbidden:
		return resp.StatusCode, data, fmt.Errorf("%w (keine Admin-Rechte)", ErrAuth)
	case http.StatusNotFound:
		return resp.StatusCode, data, ErrNotSupported
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, data, fmt.Errorf("HTTP %d von %s", resp.StatusCode, path)
	}
	return resp.StatusCode, data, nil
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) jsonCall(ctx context.Context, cgi, method string, params any, auth bool, out any) error {
	req := map[string]any{"apiVersion": "1.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)
	_, data, err := c.do(ctx, http.MethodPost, "/axis-cgi/"+cgi, body, "application/json", auth)
	if err != nil {
		return err
	}
	var resp struct {
		Data  json.RawMessage `json:"data"`
		Error *apiError       `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("%s: ungültige Antwort: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s (Code %d)", method, resp.Error.Message, resp.Error.Code)
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// ---------------------------------------------------------------- Info

// BasicInfo liest Modell, Seriennummer und Firmware. Ohne Zugangsdaten werden nur
// die frei lesbaren Eigenschaften abgefragt; neuere Firmware sperrt diese, sobald ein
// Benutzer angelegt ist – dann wird mit Anmeldung gelesen (ggf. über param.cgi).
func (c *Client) BasicInfo(ctx context.Context) (Device, error) {
	var out struct {
		PropertyList map[string]string `json:"propertyList"`
	}
	err := c.jsonCall(ctx, "basicdeviceinfo.cgi", "getAllUnrestrictedProperties", nil, false, &out)
	if errors.Is(err, ErrAuth) && c.user != "" {
		err = c.jsonCall(ctx, "basicdeviceinfo.cgi", "getAllProperties", nil, true, &out)
	}
	if errors.Is(err, ErrNotSupported) && c.user != "" {
		return c.basicInfoLegacy(ctx)
	}
	if err != nil {
		return Device{}, err
	}
	p := out.PropertyList
	model := p["ProdFullName"]
	if model == "" {
		model = p["ProdNbr"]
	}
	return Device{Model: model, Firmware: p["Version"], Serial: NormMAC(p["SerialNumber"]), Online: true}, nil
}

func (c *Client) basicInfoLegacy(ctx context.Context) (Device, error) {
	p, err := c.params(ctx, "Brand,Properties.Firmware,Properties.System")
	if err != nil {
		return Device{}, err
	}
	model := p["Brand.ProdFullName"]
	if model == "" {
		model = p["Brand.ProdNbr"]
	}
	return Device{Model: model, Firmware: p["Properties.Firmware.Version"],
		Serial: NormMAC(p["Properties.System.SerialNumber"]), Online: true}, nil
}

// params liest param.cgi-Gruppen und liefert Schlüssel ohne „root.“-Präfix.
func (c *Client) params(ctx context.Context, groups string) (map[string]string, error) {
	_, data, err := c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?action=list&group="+url.QueryEscape(groups), nil, "", true)
	if err != nil {
		return nil, err
	}
	p := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			p[strings.TrimPrefix(k, "root.")] = v
		}
	}
	return p, nil
}

// NeedsInitialUser: Gerät im Werkszustand (noch kein Benutzer angelegt)?
// Heuristik wie in der AXIS IP Utility: pwdgrp.cgi antwortet dann ohne Anmeldung.
func (c *Client) NeedsInitialUser(ctx context.Context) (bool, error) {
	code, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/pwdgrp.cgi?action=get", nil, "", false)
	if errors.Is(err, ErrAuth) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return code == http.StatusOK, nil
}

// CheckLogin prüft die Zugangsdaten.
func (c *Client) CheckLogin(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?action=list&group=Brand", nil, "", true)
	return err
}

// ---------------------------------------------------------------- Benutzer

// CreateInitialUser legt den ersten Administrator an (Werkszustand).
func (c *Client) CreateInitialUser(ctx context.Context, user, pass string) error {
	grp := "users"
	if user == "root" {
		grp = "root"
	}
	form := url.Values{"action": {"add"}, "user": {user}, "pwd": {pass}, "grp": {grp},
		"sgrp": {"admin:operator:viewer:ptz"}}
	_, data, err := c.do(ctx, http.MethodPost, "/axis-cgi/pwdgrp.cgi", []byte(form.Encode()),
		"application/x-www-form-urlencoded", false)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(string(data)), "error") {
		return nil
	}
	// Ältere Firmware: root existiert schon ohne Passwort -> update
	form = url.Values{"action": {"update"}, "user": {user}, "pwd": {pass}}
	_, data, err = c.do(ctx, http.MethodPost, "/axis-cgi/pwdgrp.cgi", []byte(form.Encode()),
		"application/x-www-form-urlencoded", false)
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(string(data)), "error") {
		return errors.New(strings.TrimSpace(string(data)))
	}
	return nil
}

// ---------------------------------------------------------------- Netzwerk

// NetworkConfig ist die vereinfachte IPv4-/Hostname-/DNS-Konfiguration.
type NetworkConfig struct {
	DHCP             bool
	Address          string
	Prefix           int
	Gateway          string
	Hostname         string
	HostnameFromDHCP bool
	DNSFromDHCP      bool
	DNS              []string
	DeviceName       string
	Legacy           bool // Gerät kennt nur param.cgi
}

// Validate prüft die Eingaben und normalisiert sie.
func (n *NetworkConfig) Validate() error {
	if !n.DHCP {
		a, err := netip.ParseAddr(n.Address)
		if err != nil || !a.Is4() {
			return fmt.Errorf("ungültige IP-Adresse %q", n.Address)
		}
		if n.Prefix < 1 || n.Prefix > 32 {
			return fmt.Errorf("ungültiges Präfix /%d", n.Prefix)
		}
		if n.Gateway != "" {
			g, err := netip.ParseAddr(n.Gateway)
			if err != nil || !g.Is4() {
				return fmt.Errorf("ungültiges Gateway %q", n.Gateway)
			}
			if !netip.PrefixFrom(a, n.Prefix).Masked().Contains(g) {
				return errors.New("Gateway liegt nicht im Subnetz")
			}
		}
	}
	if !n.HostnameFromDHCP && n.Hostname == "" {
		return errors.New("Hostname fehlt")
	}
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			return fmt.Errorf("ungültiger DNS-Server %q", d)
		}
	}
	return nil
}

// ParsePrefix akzeptiert „24“, „/24“ oder „255.255.255.0“.
func ParsePrefix(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "/")
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 32 {
		return n, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return 0, fmt.Errorf("ungültige Subnetzmaske %q", s)
	}
	b := a.As4()
	m := net.IPv4Mask(b[0], b[1], b[2], b[3])
	ones, bits := m.Size()
	if bits == 0 {
		return 0, fmt.Errorf("ungültige Subnetzmaske %q", s)
	}
	return ones, nil
}

// PrefixToMask wandelt 24 in "255.255.255.0".
func PrefixToMask(p int) string {
	m := net.CIDRMask(p, 32)
	return net.IP(m).String()
}

// GetNetwork liest die aktuelle Netzwerkkonfiguration.
func (c *Client) GetNetwork(ctx context.Context) (NetworkConfig, error) {
	var d struct {
		System struct {
			Hostname struct {
				UseDhcpHostname bool   `json:"useDhcpHostname"`
				StaticHostname  string `json:"staticHostname"`
				Hostname        string `json:"hostname"`
			} `json:"hostname"`
			Resolver struct {
				UseDhcpResolverInfo bool     `json:"useDhcpResolverInfo"`
				StaticNameServers   []string `json:"staticNameServers"`
				NameServers         []string `json:"nameServers"`
			} `json:"resolver"`
		} `json:"system"`
		Devices []struct {
			Name string `json:"name"`
			IPv4 struct {
				ConfigurationMode           string `json:"configurationMode"`
				StaticDefaultRouter         string `json:"staticDefaultRouter"`
				DefaultRouter               string `json:"defaultRouter"`
				StaticAddressConfigurations []struct {
					Address      string `json:"address"`
					PrefixLength int    `json:"prefixLength"`
				} `json:"staticAddressConfigurations"`
				Addresses []struct {
					Address      string `json:"address"`
					PrefixLength int    `json:"prefixLength"`
				} `json:"addresses"`
			} `json:"IPv4"`
		} `json:"devices"`
	}
	err := c.jsonCall(ctx, "network_settings.cgi", "getNetworkInfo", nil, true, &d)
	if errors.Is(err, ErrNotSupported) {
		return c.getNetworkLegacy(ctx)
	}
	if err != nil {
		return NetworkConfig{}, err
	}
	cfg := NetworkConfig{DHCP: true, Prefix: 24, DeviceName: "eth0"}
	if len(d.Devices) > 0 {
		dev := d.Devices[0]
		v4 := dev.IPv4
		if dev.Name != "" {
			cfg.DeviceName = dev.Name
		}
		cfg.DHCP = v4.ConfigurationMode != "static"
		if !cfg.DHCP && len(v4.StaticAddressConfigurations) > 0 {
			cfg.Address = v4.StaticAddressConfigurations[0].Address
			cfg.Prefix = v4.StaticAddressConfigurations[0].PrefixLength
		} else {
			for _, a := range v4.Addresses {
				if !strings.HasPrefix(a.Address, "169.254.") {
					cfg.Address, cfg.Prefix = a.Address, a.PrefixLength
					break
				}
			}
		}
		cfg.Gateway = v4.StaticDefaultRouter
		if cfg.Gateway == "" {
			cfg.Gateway = v4.DefaultRouter
		}
	}
	h := d.System.Hostname
	cfg.HostnameFromDHCP = h.UseDhcpHostname
	cfg.Hostname = h.StaticHostname
	if cfg.Hostname == "" {
		cfg.Hostname = h.Hostname
	}
	r := d.System.Resolver
	cfg.DNSFromDHCP = r.UseDhcpResolverInfo
	cfg.DNS = r.StaticNameServers
	if len(cfg.DNS) == 0 {
		cfg.DNS = r.NameServers
	}
	return cfg, nil
}

// SetNetwork schreibt die Konfiguration. Die IP wird zuletzt gesetzt, weil das
// Gerät danach unter der alten Adresse evtl. nicht mehr antwortet.
func (c *Client) SetNetwork(ctx context.Context, cfg NetworkConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Legacy {
		return c.setNetworkLegacy(ctx, cfg)
	}
	if err := c.jsonCall(ctx, "network_settings.cgi", "setHostnameConfiguration", map[string]any{
		"useDhcpHostname": cfg.HostnameFromDHCP, "staticHostname": cfg.Hostname}, true, nil); err != nil {
		return err
	}
	res := map[string]any{"useDhcpResolverInfo": cfg.DNSFromDHCP}
	if !cfg.DNSFromDHCP {
		res["staticNameServers"] = nonNil(cfg.DNS)
	}
	if err := c.jsonCall(ctx, "network_settings.cgi", "setResolverConfiguration", res, true, nil); err != nil {
		return err
	}
	p := map[string]any{"deviceName": cfg.DeviceName, "configurationMode": "dhcp"}
	if !cfg.DHCP {
		p["configurationMode"] = "static"
		p["staticDefaultRouter"] = cfg.Gateway
		p["staticAddressConfigurations"] = []map[string]any{{"address": cfg.Address, "prefixLength": cfg.Prefix}}
	}
	return ignoreDisconnect(c.jsonCall(ctx, "network_settings.cgi", "setIPv4AddressConfiguration", p, true, nil))
}

func (c *Client) getNetworkLegacy(ctx context.Context) (NetworkConfig, error) {
	_, data, err := c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?action=list&group=Network", nil, "", true)
	if err != nil {
		return NetworkConfig{}, err
	}
	p := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			p[strings.TrimPrefix(k, "root.Network.")] = v
		}
	}
	cfg := NetworkConfig{Legacy: true, DeviceName: "eth0"}
	cfg.DHCP = p["BootProto"] != "none"
	cfg.Address = p["IPAddress"]
	cfg.Prefix, _ = ParsePrefix(p["SubnetMask"])
	cfg.Gateway = p["DefaultRouter"]
	cfg.Hostname = p["HostName"]
	cfg.HostnameFromDHCP = p["VolatileHostName.ObtainFromDHCP"] != "no"
	cfg.DNSFromDHCP = p["Resolver.ObtainFromDHCP"] != "no"
	for _, k := range []string{"Resolver.NameServer1", "Resolver.NameServer2"} {
		if p[k] != "" {
			cfg.DNS = append(cfg.DNS, p[k])
		}
	}
	return cfg, nil
}

func (c *Client) setNetworkLegacy(ctx context.Context, cfg NetworkConfig) error {
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	q := url.Values{
		"action":           {"update"},
		"Network.HostName": {cfg.Hostname},
		"Network.VolatileHostName.ObtainFromDHCP": {yn(cfg.HostnameFromDHCP)},
		"Network.Resolver.ObtainFromDHCP":         {yn(cfg.DNSFromDHCP)},
		"Network.BootProto":                       {map[bool]string{true: "dhcp", false: "none"}[cfg.DHCP]},
	}
	for i, k := range []string{"Network.Resolver.NameServer1", "Network.Resolver.NameServer2"} {
		v := ""
		if i < len(cfg.DNS) {
			v = cfg.DNS[i]
		}
		q.Set(k, v)
	}
	if !cfg.DHCP {
		q.Set("Network.IPAddress", cfg.Address)
		q.Set("Network.SubnetMask", PrefixToMask(cfg.Prefix))
		q.Set("Network.DefaultRouter", cfg.Gateway)
	}
	_, data, err := c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?"+q.Encode(), nil, "", true)
	if err = ignoreDisconnect(err); err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(string(data)), "error") {
		return errors.New(strings.TrimSpace(string(data)))
	}
	return nil
}

// ---------------------------------------------------------------- Zeit

// SetTime setzt Zeitzone (IANA, z. B. "Europe/Vienna") und/oder die aktuelle Uhrzeit.
func (c *Client) SetTime(ctx context.Context, tz string, syncNow bool) error {
	if tz != "" {
		if err := c.jsonCall(ctx, "time.cgi", "setTimeZone", map[string]string{"timeZone": tz}, true, nil); err != nil {
			return err
		}
	}
	if !syncNow {
		return nil
	}
	now := time.Now().UTC()
	err := c.jsonCall(ctx, "time.cgi", "setDateTime",
		map[string]string{"dateTime": now.Format("2006-01-02T15:04:05Z")}, true, nil)
	if errors.Is(err, ErrNotSupported) { // ältere Firmware
		q := fmt.Sprintf("/axis-cgi/date.cgi?action=set&year=%d&month=%d&day=%d&hour=%d&minute=%d&second=%d",
			now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second())
		_, _, err = c.do(ctx, http.MethodGet, q, nil, "", true)
	}
	return err
}

// ---------------------------------------------------------------- NTP

// NTPConfig beschreibt die NTP-Client-Einstellungen.
type NTPConfig struct {
	Enabled  bool
	FromDHCP bool     // Server per DHCP beziehen
	Servers  []string // statische Server
	Active   []string // aktuell verwendete Server (nur lesen)
	Synced   *bool    // nil = unbekannt
	Legacy   bool     // Gerät kennt nur param.cgi
}

// GetNTP liest die NTP-Konfiguration.
func (c *Client) GetNTP(ctx context.Context) (NTPConfig, error) {
	type client struct {
		Enabled       *bool    `json:"enabled"`
		ServersSource string   `json:"serversSource"`
		StaticServers []string `json:"staticServers"`
		Servers       []string `json:"servers"`
		Synced        *bool    `json:"synced"`
	}
	var d struct {
		client
		Client *client `json:"client"`
	}
	err := c.jsonCall(ctx, "ntp.cgi", "getNTPInfo", nil, true, &d)
	if errors.Is(err, ErrNotSupported) {
		return c.getNTPLegacy(ctx)
	}
	if err != nil {
		return NTPConfig{}, err
	}
	cl := d.client
	if d.Client != nil {
		cl = *d.Client
	}
	cfg := NTPConfig{
		Enabled:  cl.Enabled == nil || *cl.Enabled,
		FromDHCP: strings.EqualFold(cl.ServersSource, "DHCP"),
		Servers:  cl.StaticServers,
		Active:   cl.Servers,
		Synced:   cl.Synced,
	}
	if cfg.Synced == nil {
		cfg.Synced = d.Synced
	}
	return cfg, nil
}

// SetNTP schreibt die NTP-Konfiguration.
func (c *Client) SetNTP(ctx context.Context, cfg NTPConfig) error {
	for _, s := range cfg.Servers {
		if strings.ContainsAny(s, " ,;/") || s == "" {
			return fmt.Errorf("ungültiger NTP-Server %q", s)
		}
	}
	if cfg.Enabled && !cfg.FromDHCP && len(cfg.Servers) == 0 {
		return errors.New("mindestens einen NTP-Server angeben oder „per DHCP“ wählen")
	}
	if cfg.Legacy {
		return c.setNTPLegacy(ctx, cfg)
	}
	p := map[string]any{"enabled": cfg.Enabled, "serversSource": "static", "staticServers": nonNil(cfg.Servers)}
	if cfg.FromDHCP {
		p["serversSource"] = "DHCP"
	}
	err := c.jsonCall(ctx, "ntp.cgi", "setNTPClientConfiguration", p, true, nil)
	if errors.Is(err, ErrNotSupported) {
		return c.setNTPLegacy(ctx, cfg)
	}
	return err
}

func (c *Client) getNTPLegacy(ctx context.Context) (NTPConfig, error) {
	p, err := c.params(ctx, "Time")
	if err != nil {
		return NTPConfig{}, err
	}
	cfg := NTPConfig{Legacy: true}
	cfg.Enabled = strings.EqualFold(p["Time.SyncSource"], "NTP")
	cfg.FromDHCP = p["Time.ObtainFromDHCP"] == "yes"
	for _, s := range strings.FieldsFunc(p["Time.NTP.Server"], func(r rune) bool { return r == ',' || r == ' ' }) {
		cfg.Servers = append(cfg.Servers, s)
	}
	return cfg, nil
}

func (c *Client) setNTPLegacy(ctx context.Context, cfg NTPConfig) error {
	src := "None"
	if cfg.Enabled {
		src = "NTP"
	}
	q := url.Values{
		"action":              {"update"},
		"Time.SyncSource":     {src},
		"Time.ObtainFromDHCP": {map[bool]string{true: "yes", false: "no"}[cfg.FromDHCP]},
	}
	if len(cfg.Servers) > 0 {
		q.Set("Time.NTP.Server", cfg.Servers[0]) // alte Firmware kennt nur einen Server
	}
	_, data, err := c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?"+q.Encode(), nil, "", true)
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(string(data)), "error") {
		return errors.New(strings.TrimSpace(string(data)))
	}
	return nil
}

// ---------------------------------------------------------------- Vorschau

// Snapshot holt ein JPEG-Standbild. width > 0 fordert eine passende Auflösung an;
// lehnt das Gerät sie ab, wird die Standardauflösung verwendet.
func (c *Client) Snapshot(ctx context.Context, width int) ([]byte, error) {
	path := "/axis-cgi/jpg/image.cgi"
	if width > 0 {
		if _, data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?resolution=%dx%d", path, width, width*9/16), nil, "", true); err == nil && isJPEG(data) {
			return data, nil
		} else if errors.Is(err, ErrAuth) {
			return nil, err
		}
	}
	_, data, err := c.do(ctx, http.MethodGet, path, nil, "", true)
	if err != nil {
		return nil, err
	}
	if !isJPEG(data) {
		return nil, errors.New("Gerät lieferte kein Bild (Kamera deaktiviert oder kein Videokanal?)")
	}
	return data, nil
}

func isJPEG(b []byte) bool { return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 }

// ---------------------------------------------------------------- Sonstiges

// Restart startet das Gerät neu.
func (c *Client) Restart(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/restart.cgi", nil, "", true)
	return ignoreDisconnect(err)
}

func ignoreDisconnect(err error) error {
	var ne net.Error
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) || strings.Contains(err.Error(), "connection reset") {
		return nil
	}
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
