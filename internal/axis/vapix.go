// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"sync/atomic"
	"time"
)

// ErrAuth: credentials missing, wrong or without admin rights.
var ErrAuth = errors.New("wrong user name or password")

// ErrHasUser: initial password impossible, the device already has a user.
var ErrHasUser = errors.New("device already has a user – the password can only be changed when logged in (web interface)")

// ErrSetupUnknown: the factory state could not be determined.
var ErrSetupUnknown = errors.New("factory state cannot be determined")

// ErrNoUser: login impossible because the device is still in factory state (is also ErrAuth).
var ErrNoUser error = noUserError{}

type noUserError struct{}

func (noUserError) Error() string {
	return "device has no user yet – set a password first"
}
func (noUserError) Is(target error) bool { return target == ErrAuth }

// ErrNotSupported: the device doesn't know the (new) API.
var ErrNotSupported = errors.New("not supported by the firmware")

// ErrNoHTTPS: "HTTPS only" is enabled, but the device doesn't speak HTTPS.
var ErrNoHTTPS = errors.New("device is not reachable via HTTPS (setting \"HTTPS only\")")

// ---------------------------------------------------------------- HTTPS detection

// httpsPort is the port HTTPS is expected on (overridable in tests).
var httpsPort = 443

var (
	httpsOnly   atomic.Bool
	schemeMu    sync.Mutex
	schemeCache = map[string]schemeEntry{}
)

type schemeEntry struct {
	https bool
	at    time.Time
}

const schemeTTL = CheckInterval // like the background check

// SetHTTPSOnly: true = never fall back to unencrypted HTTP.
func SetHTTPSOnly(on bool) {
	httpsOnly.Store(on)
}

// HTTPSOnly returns the current setting.
func HTTPSOnly() bool { return httpsOnly.Load() }

// Self-signed camera certificates are accepted (not verified): the connection is
// encrypted but not protected against an active attacker.
func tlsConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

// ProbeHTTPS checks via TLS handshake whether host speaks HTTPS. The result is cached briefly.
func ProbeHTTPS(ctx context.Context, host string) bool {
	schemeMu.Lock()
	e, ok := schemeCache[host]
	schemeMu.Unlock()
	if ok && time.Since(e.at) < schemeTTL {
		return e.https
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: tlsConfig()}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(httpsPort)))
	if err == nil {
		conn.Close()
	} else if ctx.Err() != nil {
		return false // cancelled – don't remember anything
	}
	schemeMu.Lock()
	schemeCache[host] = schemeEntry{https: err == nil, at: time.Now()}
	schemeMu.Unlock()
	return err == nil
}

// ---------------------------------------------------------------- Client

// Client speaks VAPIX with a single device – via HTTPS if the device offers it,
// otherwise (except with "HTTPS only") via HTTP.
type Client struct {
	host       string
	port       int // HTTP port
	user, pass string
	hc         *http.Client

	mu     sync.Mutex
	base   string // determined on the first call
	secure bool
	chal   *challenge
}

// NewClient creates a client. port is the HTTP port (0 = 80).
// user may be empty (open endpoints only).
func NewClient(host string, port int, user, pass string) *Client {
	if port == 0 {
		port = 80
	}
	return &Client{
		host: host, port: port,
		user: user, pass: pass,
		hc: &http.Client{
			Timeout: 8 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil, // cameras are local, never via a proxy
				TLSClientConfig: tlsConfig(),
			},
		},
	}
}

// baseURL chooses the protocol on the first call.
func (c *Client) baseURL(ctx context.Context) (string, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base != "" {
		return base, nil
	}
	secure := ProbeHTTPS(ctx, c.host)
	switch {
	case secure:
		base = "https://" + net.JoinHostPort(c.host, strconv.Itoa(httpsPort))
	case HTTPSOnly():
		return "", ErrNoHTTPS
	default:
		base = "http://" + net.JoinHostPort(c.host, strconv.Itoa(c.port))
	}
	c.mu.Lock()
	c.base, c.secure = base, secure
	c.mu.Unlock()
	return base, nil
}

// Secure reports whether the client talks to the device encrypted (HTTPS).
func (c *Client) Secure(ctx context.Context) bool {
	_, err := c.baseURL(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return err == nil && c.secure
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, ctype string, auth bool) (int, []byte, error) {
	base, err := c.baseURL(ctx)
	if err != nil {
		return 0, nil, err
	}
	send := func(authz string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
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
		return resp.StatusCode, data, fmt.Errorf("%w (no admin rights)", ErrAuth)
	case http.StatusNotFound:
		return resp.StatusCode, data, ErrNotSupported
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, data, fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	return resp.StatusCode, data, nil
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) jsonCall(ctx context.Context, cgi, method string, params any, auth bool, out any) error {
	// "context" is optional according to VAPIX, but some firmware (e.g. P3225-VE Mk II)
	// rejects systemready.cgi without it with "Invalid POST data".
	req := map[string]any{"apiVersion": "1.0", "context": "axis-ip-tool", "method": method}
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
		return fmt.Errorf("%s: invalid response: %w", method, err)
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

// BasicInfo reads model, serial number and firmware. Without credentials only the
// unrestricted properties are queried; newer firmware locks them once a user has been
// created – then they are read with login (via param.cgi if necessary).
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

// params reads param.cgi groups and returns keys without the "root." prefix.
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

// NeedsInitialUser: is the device in factory state (no user created yet)?
func (c *Client) NeedsInitialUser(ctx context.Context) (bool, error) {
	// systemready.cgi reports the factory state explicitly ("needsetup") and needs no
	// login. Important: AXIS OS 11 answers pwdgrp.cgi with 401 even in factory state.
	var ready struct {
		NeedSetup string `json:"needsetup"`
	}
	err := c.jsonCall(ctx, "systemready.cgi", "systemready", map[string]any{"timeout": 5}, false, &ready)
	if err == nil && (ready.NeedSetup == "yes" || ready.NeedSetup == "no") {
		return ready.NeedSetup == "yes", nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Older firmware: heuristic as in the AXIS IP Utility. Only an anonymous answer is
	// conclusive – newer firmware returns 401 in factory state too, so "unknown".
	code, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/pwdgrp.cgi?action=get", nil, "", false)
	if errors.Is(err, ErrAuth) {
		return false, ErrSetupUnknown
	}
	if err != nil {
		return false, err
	}
	return code == http.StatusOK, nil
}

// CheckLogin verifies the credentials (administrator). pwdgrp.cgi always requires an
// admin login – unlike param.cgi/basicdeviceinfo.cgi, which may answer anonymous access
// even with a wrong password.
func (c *Client) CheckLogin(ctx context.Context) error {
	if c.user == "" {
		return ErrAuth
	}
	code, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/pwdgrp.cgi?action=get", nil, "", true)
	if errors.Is(err, ErrNotSupported) {
		code, _, err = c.do(ctx, http.MethodGet, "/axis-cgi/param.cgi?action=list&group=Brand", nil, "", true)
	}
	c.mu.Lock()
	asked := c.chal != nil
	c.mu.Unlock()
	if err == nil && !asked && code == http.StatusOK {
		// answered without login → factory state, credentials cannot be verified
		return ErrNoUser
	}
	if errors.Is(err, ErrAuth) {
		// rejected – because of the password or the factory state?
		if needs, e := NewClient(c.host, c.port, "", "").NeedsInitialUser(ctx); e == nil && needs {
			return ErrNoUser
		}
	}
	return err
}

// ---------------------------------------------------------------- Users

// CreateInitialUser creates the first administrator (factory state).
func (c *Client) CreateInitialUser(ctx context.Context, user, pass string) error {
	grp := "users"
	if user == "root" {
		grp = "root"
	}
	form := url.Values{"action": {"add"}, "user": {user}, "pwd": {pass}, "grp": {grp},
		"sgrp": {"admin:operator:viewer:ptz"}}
	_, data, err := c.do(ctx, http.MethodPost, "/axis-cgi/pwdgrp.cgi", []byte(form.Encode()),
		"application/x-www-form-urlencoded", false)
	if errors.Is(err, ErrAuth) {
		return ErrHasUser
	}
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(string(data)), "error") {
		return nil
	}
	// older firmware: root already exists without password -> update
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

// ---------------------------------------------------------------- Network

// NetworkConfig is the simplified IPv4/hostname/DNS configuration.
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
	Legacy           bool // device only knows param.cgi
}

// Validate checks the input and normalizes it.
func (n *NetworkConfig) Validate() error {
	if !n.DHCP {
		a, err := netip.ParseAddr(n.Address)
		if err != nil || !a.Is4() {
			return fmt.Errorf("invalid IP address %q", n.Address)
		}
		if n.Prefix < 1 || n.Prefix > 32 {
			return fmt.Errorf("invalid prefix /%d", n.Prefix)
		}
		if n.Gateway != "" {
			g, err := netip.ParseAddr(n.Gateway)
			if err != nil || !g.Is4() {
				return fmt.Errorf("invalid gateway %q", n.Gateway)
			}
			if !netip.PrefixFrom(a, n.Prefix).Masked().Contains(g) {
				return errors.New("gateway is not in the subnet")
			}
		}
	}
	if !n.HostnameFromDHCP && n.Hostname == "" {
		return errors.New("hostname missing")
	}
	for _, d := range n.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			return fmt.Errorf("invalid DNS server %q", d)
		}
	}
	return nil
}

// ParsePrefix accepts "24", "/24" or "255.255.255.0".
func ParsePrefix(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "/")
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 32 {
		return n, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return 0, fmt.Errorf("invalid subnet mask %q", s)
	}
	b := a.As4()
	m := net.IPv4Mask(b[0], b[1], b[2], b[3])
	ones, bits := m.Size()
	if bits == 0 {
		return 0, fmt.Errorf("invalid subnet mask %q", s)
	}
	return ones, nil
}

// PrefixToMask converts 24 to "255.255.255.0".
func PrefixToMask(p int) string {
	m := net.CIDRMask(p, 32)
	return net.IP(m).String()
}

// GetNetwork reads the current network configuration.
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

// SetNetwork writes the configuration. The IP is set last because the device may no
// longer answer at the old address afterwards.
func (c *Client) SetNetwork(ctx context.Context, cfg NetworkConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Legacy {
		return c.setNetworkLegacy(ctx, cfg)
	}
	if err := c.setHostname(ctx, cfg); err != nil {
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

// setHostname sets the hostname and checks whether the device applied it. Some AXIS OS 9
// versions don't know setHostnameConfiguration or don't apply it – then param.cgi is tried.
func (c *Client) setHostname(ctx context.Context, cfg NetworkConfig) error {
	err := c.jsonCall(ctx, "network_settings.cgi", "setHostnameConfiguration", map[string]any{
		"useDhcpHostname": cfg.HostnameFromDHCP, "staticHostname": cfg.Hostname}, true, nil)
	if err == nil && c.hostnameApplied(ctx, cfg) {
		return nil
	}
	if errors.Is(err, ErrAuth) {
		return err
	}
	if perr := c.setHostnameParam(ctx, cfg); perr != nil {
		if err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
		return fmt.Errorf("hostname: %w", perr)
	}
	if !c.hostnameApplied(ctx, cfg) {
		return fmt.Errorf("device did not apply the hostname %q", cfg.Hostname)
	}
	return nil
}

// hostnameApplied reads the hostname back (unreadable = treat as applied).
func (c *Client) hostnameApplied(ctx context.Context, cfg NetworkConfig) bool {
	if cfg.HostnameFromDHCP {
		return true
	}
	got, err := c.GetNetwork(ctx)
	return err != nil || strings.EqualFold(got.Hostname, cfg.Hostname)
}

func (c *Client) setHostnameParam(ctx context.Context, cfg NetworkConfig) error {
	q := url.Values{
		"action":           {"update"},
		"Network.HostName": {cfg.Hostname},
		"Network.VolatileHostName.ObtainFromDHCP": {yesNo(cfg.HostnameFromDHCP)},
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

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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
	// hostname first and separately so it can be verified before the IP changes
	if err := c.setHostnameParam(ctx, cfg); err != nil {
		return fmt.Errorf("hostname: %w", err)
	}
	if !c.hostnameApplied(ctx, cfg) {
		return fmt.Errorf("device did not apply the hostname %q", cfg.Hostname)
	}
	q := url.Values{
		"action":                          {"update"},
		"Network.Resolver.ObtainFromDHCP": {yesNo(cfg.DNSFromDHCP)},
		"Network.BootProto":               {map[bool]string{true: "dhcp", false: "none"}[cfg.DHCP]},
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

// ---------------------------------------------------------------- Time

// SetTime sets the time zone (IANA, e.g. "Europe/Vienna") and/or the current time.
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
	if errors.Is(err, ErrNotSupported) { // older firmware
		q := fmt.Sprintf("/axis-cgi/date.cgi?action=set&year=%d&month=%d&day=%d&hour=%d&minute=%d&second=%d",
			now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second())
		_, _, err = c.do(ctx, http.MethodGet, q, nil, "", true)
	}
	return err
}

// ---------------------------------------------------------------- NTP

// NTPConfig describes the NTP client settings.
type NTPConfig struct {
	Enabled  bool
	FromDHCP bool     // obtain servers via DHCP
	Servers  []string // static servers
	Active   []string // servers currently in use (read only)
	Synced   *bool    // nil = unknown
	Legacy   bool     // device only knows param.cgi
}

// GetNTP reads the NTP configuration.
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

// SetNTP writes the NTP configuration.
func (c *Client) SetNTP(ctx context.Context, cfg NTPConfig) error {
	for _, s := range cfg.Servers {
		if strings.ContainsAny(s, " ,;/") || s == "" {
			return fmt.Errorf("invalid NTP server %q", s)
		}
	}
	if cfg.Enabled && !cfg.FromDHCP && len(cfg.Servers) == 0 {
		return errors.New("specify at least one NTP server or choose \"via DHCP\"")
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
		q.Set("Time.NTP.Server", cfg.Servers[0]) // old firmware only knows one server
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

// ---------------------------------------------------------------- Preview

// Snapshot fetches a JPEG still image. width > 0 requests a matching resolution;
// if the device rejects it, the default resolution is used.
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
		return nil, errors.New("device returned no image (camera disabled or no video channel?)")
	}
	return data, nil
}

func isJPEG(b []byte) bool { return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 }

// ---------------------------------------------------------------- Misc

// Restart reboots the device.
func (c *Client) Restart(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/axis-cgi/restart.cgi", nil, "", true)
	return ignoreDisconnect(err)
}

// FactoryReset resets the device to factory defaults (users, passwords and all settings)
// and reboots it. keepNetwork keeps the IP settings (factorydefault.cgi); otherwise
// everything is reset (hardfactorydefault.cgi) – the device then obtains an address via
// DHCP or uses 192.168.0.90.
func (c *Client) FactoryReset(ctx context.Context, keepNetwork bool) error {
	path := "/axis-cgi/hardfactorydefault.cgi"
	if keepNetwork {
		path = "/axis-cgi/factorydefault.cgi"
	}
	_, _, err := c.do(ctx, http.MethodGet, path, nil, "", true)
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
