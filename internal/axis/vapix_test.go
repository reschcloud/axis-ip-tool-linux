// SPDX-License-Identifier: GPL-3.0-or-later

package axis

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeCamera simuliert die benötigten VAPIX-Endpunkte inkl. Digest-Auth.
type fakeCamera struct {
	mu       sync.Mutex
	users    map[string]string
	calls    []string
	jsonBody []map[string]any
	locked   bool // basicdeviceinfo nur mit Anmeldung (AXIS OS 12 nach Einrichtung)
	ntp      map[string]any
}

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

func (f *fakeCamera) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := parseParams(strings.TrimPrefix(h, "Digest "))
	pass, ok := f.users[p["username"]]
	if !ok {
		return false
	}
	ha1 := md5hex(p["username"] + ":AXIS_TEST:" + pass)
	ha2 := md5hex(r.Method + ":" + p["uri"])
	want := md5hex(strings.Join([]string{ha1, p["nonce"], p["nc"], p["cnonce"], p["qop"], ha2}, ":"))
	return p["response"] == want && p["nonce"] == "abc123"
}

func (f *fakeCamera) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	open := (r.URL.Path == "/axis-cgi/basicdeviceinfo.cgi" && !f.locked) ||
		(r.URL.Path == "/axis-cgi/pwdgrp.cgi" && len(f.users) == 0)
	if !open && !f.authorized(r) {
		w.Header().Add("WWW-Authenticate", `Basic realm="AXIS_TEST"`)
		w.Header().Add("WWW-Authenticate", `Digest realm="AXIS_TEST", nonce="abc123", algorithm=MD5, qop="auth"`)
		w.WriteHeader(401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/axis-cgi/basicdeviceinfo.cgi":
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"propertyList": map[string]string{
			"ProdFullName": "AXIS M3045-V Network Camera", "SerialNumber": "ACCC8E112233", "Version": "11.11.73"}}})
	case "/axis-cgi/pwdgrp.cgi":
		if r.Method == http.MethodPost {
			v, _ := url.ParseQuery(string(body))
			f.users[v.Get("user")] = v.Get("pwd")
			io.WriteString(w, "Created account "+v.Get("user")+".")
		} else {
			io.WriteString(w, "root\n")
		}
	case "/axis-cgi/network_settings.cgi":
		var req map[string]any
		json.Unmarshal(body, &req)
		f.jsonBody = append(f.jsonBody, req)
		if req["method"] == "getNetworkInfo" {
			io.WriteString(w, `{"data":{"system":{"hostname":{"useDhcpHostname":false,"staticHostname":"cam1"},
			  "resolver":{"useDhcpResolverInfo":true,"nameServers":["10.0.0.1"]}},
			  "devices":[{"name":"eth0","IPv4":{"configurationMode":"static","staticDefaultRouter":"10.0.0.1",
			  "staticAddressConfigurations":[{"address":"10.0.0.50","prefixLength":24}]}}]}}`)
			return
		}
		io.WriteString(w, `{"data":{}}`)
	case "/axis-cgi/ntp.cgi":
		var req map[string]any
		json.Unmarshal(body, &req)
		if req["method"] == "setNTPClientConfiguration" {
			f.ntp = req["params"].(map[string]any)
			io.WriteString(w, `{"data":{}}`)
			return
		}
		io.WriteString(w, `{"data":{"client":{"enabled":true,"serversSource":"static",
		  "staticServers":["pool.ntp.org"],"servers":["pool.ntp.org"],"synced":true}}}`)
	case "/axis-cgi/jpg/image.cgi":
		if r.URL.Query().Get("resolution") != "" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0})
	case "/axis-cgi/time.cgi":
		io.WriteString(w, `{"error":{"code":2002,"message":"Invalid time zone"}}`)
	default:
		w.WriteHeader(404)
	}
}

func startFake(t *testing.T) (*fakeCamera, string, int) {
	f := &fakeCamera{users: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return f, host, p
}

func TestWorkflow(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)

	anon := NewClient(host, port, "", "")
	info, err := anon.BasicInfo(ctx)
	if err != nil || info.Serial != "ACCC8E112233" || info.Firmware != "11.11.73" {
		t.Fatalf("BasicInfo = %+v, %v", info, err)
	}
	if needs, err := anon.NeedsInitialUser(ctx); err != nil || !needs {
		t.Fatalf("NeedsInitialUser vor Setup = %v, %v", needs, err)
	}
	if err := anon.CreateInitialUser(ctx, "root", "geheim123"); err != nil {
		t.Fatal(err)
	}
	if needs, _ := anon.NeedsInitialUser(ctx); needs {
		t.Fatal("NeedsInitialUser nach Setup = true")
	}

	if err := NewClient(host, port, "root", "falsch").CheckLogin(ctx); err != ErrAuth {
		t.Fatalf("falsches Passwort: %v", err)
	}

	c := NewClient(host, port, "root", "geheim123")
	cfg, err := c.GetNetwork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DHCP || cfg.Address != "10.0.0.50" || cfg.Prefix != 24 || cfg.Hostname != "cam1" || !cfg.DNSFromDHCP {
		t.Fatalf("GetNetwork = %+v", cfg)
	}
	cfg.Address = "10.0.0.60"
	if err := c.SetNetwork(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	last := f.jsonBody[len(f.jsonBody)-1]
	if last["method"] != "setIPv4AddressConfiguration" {
		t.Fatalf("letzter Aufruf %v", last["method"])
	}
	addrs := last["params"].(map[string]any)["staticAddressConfigurations"].([]any)
	if addrs[0].(map[string]any)["address"] != "10.0.0.60" {
		t.Fatalf("Adresse nicht übernommen: %v", addrs)
	}

	if err := c.SetTime(ctx, "Mars/Olympus", false); err == nil || !strings.Contains(err.Error(), "Invalid time zone") {
		t.Fatalf("API-Fehler nicht gemeldet: %v", err)
	}
}

func TestLockedInfoNTPSnapshot(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)
	f.users["root"] = "geheim123"
	f.locked = true

	if _, err := NewClient(host, port, "", "").BasicInfo(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("anonym gesperrt: %v", err)
	}
	c := NewClient(host, port, "root", "geheim123")
	info, err := c.BasicInfo(ctx)
	if err != nil || info.Firmware != "11.11.73" {
		t.Fatalf("BasicInfo mit Anmeldung = %+v, %v", info, err)
	}

	n, err := c.GetNTP(ctx)
	if err != nil || !n.Enabled || n.FromDHCP || len(n.Servers) != 1 || n.Synced == nil || !*n.Synced {
		t.Fatalf("GetNTP = %+v, %v", n, err)
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true}); err == nil {
		t.Fatal("NTP ohne Server akzeptiert")
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true, Servers: []string{"0.at.pool.ntp.org", "10.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	if f.ntp["serversSource"] != "static" || len(f.ntp["staticServers"].([]any)) != 2 {
		t.Fatalf("SetNTP sendete %v", f.ntp)
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true, FromDHCP: true}); err != nil || f.ntp["serversSource"] != "DHCP" {
		t.Fatalf("SetNTP DHCP: %v %v", f.ntp, err)
	}

	img, err := c.Snapshot(ctx, 640)
	if err != nil || !isJPEG(img) {
		t.Fatalf("Snapshot = %v, %v", img, err)
	}
	if _, err := NewClient(host, port, "root", "falsch").Snapshot(ctx, 640); !errors.Is(err, ErrAuth) {
		t.Fatalf("Snapshot falsches Passwort: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := []NetworkConfig{
		{Address: "10.0.0.300", Prefix: 24, HostnameFromDHCP: true},
		{Address: "10.0.0.5", Prefix: 24, Gateway: "10.0.1.1", HostnameFromDHCP: true},
		{DHCP: true, HostnameFromDHCP: false},
	}
	for _, c := range bad {
		if c.Validate() == nil {
			t.Errorf("erwartet Fehler für %+v", c)
		}
	}
	ok := NetworkConfig{Address: "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1", HostnameFromDHCP: true}
	if err := ok.Validate(); err != nil {
		t.Error(err)
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]int{"24": 24, "/16": 16, "255.255.255.0": 24, "255.255.0.0": 16} {
		if got, err := ParsePrefix(in); err != nil || got != want {
			t.Errorf("ParsePrefix(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParsePrefix("255.0.255.0"); err == nil {
		t.Error("ungültige Maske akzeptiert")
	}
	if PrefixToMask(20) != "255.255.240.0" {
		t.Error(PrefixToMask(20))
	}
	if NormMAC("uuid:Upnp-BasicDevice-1_0-ac:cc:8e:11:22:33") != "ACCC8E112233" {
		t.Error("NormMAC")
	}
	if m, err := ParseMAC("ACCC8E112233"); err != nil || m.String() != "ac:cc:8e:11:22:33" {
		t.Error("ParseMAC", m, err)
	}
	l, m := splitServiceName("AXIS P1448-LE - ACCC8E123456")
	if l != "AXIS P1448-LE" || m != "ACCC8E123456" {
		t.Error("splitServiceName", l, m)
	}
	h := ParseSSDP("HTTP/1.1 200 OK\r\nLOCATION: http://1.2.3.4:45895/rootdesc1.xml\r\nST: upnp:rootdevice\r\n\r\n")
	if h["location"] != "http://1.2.3.4:45895/rootdesc1.xml" {
		t.Error("ParseSSDP", h)
	}
	r, err := ParseRootDesc([]byte(`<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0">
	  <device><friendlyName>AXIS M3045-V - ACCC8E112233</friendlyName><modelName>AXIS M3045-V</modelName>
	  <serialNumber>ACCC8E112233</serialNumber><presentationURL>http://1.2.3.4:80/</presentationURL></device></root>`))
	if err != nil || r.ModelName != "AXIS M3045-V" || r.SerialNumber != "ACCC8E112233" {
		t.Error("ParseRootDesc", r, err)
	}
}

func resetScheme(t *testing.T, port int) {
	old := httpsPort
	httpsPort = port
	schemeMu.Lock()
	schemeCache = map[string]schemeEntry{}
	schemeMu.Unlock()
	t.Cleanup(func() {
		httpsPort = old
		SetHTTPSOnly(false)
		schemeMu.Lock()
		schemeCache = map[string]schemeEntry{}
		schemeMu.Unlock()
	})
}

func TestHTTPSPreferred(t *testing.T) {
	ctx := context.Background()
	f := &fakeCamera{users: map[string]string{"root": "geheim123"}}
	srv := httptest.NewTLSServer(f) // selbst signiert, wie bei den Kameras
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	p, _ := strconv.Atoi(port)
	resetScheme(t, p)

	// HTTP-Port zeigt ins Leere – es darf nur HTTPS verwendet werden
	c := NewClient(host, 1, "root", "geheim123")
	info, err := c.BasicInfo(ctx)
	if err != nil || info.Firmware != "11.11.73" {
		t.Fatalf("BasicInfo über HTTPS = %+v, %v", info, err)
	}
	if !c.Secure(ctx) {
		t.Fatal("Client meldet keine HTTPS-Verbindung")
	}
	if d := (Device{IP: host, HTTPS: &[]bool{true}[0]}); !strings.HasPrefix(d.WebURL(), "https://") {
		t.Fatal(d.WebURL())
	}
}

func TestHTTPFallbackAndHTTPSOnly(t *testing.T) {
	ctx := context.Background()
	_, host, port := startFake(t) // nur HTTP
	// Port für HTTPS: ein sicher geschlossener Port
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	resetScheme(t, closed)

	c := NewClient(host, port, "", "")
	if _, err := c.BasicInfo(ctx); err != nil {
		t.Fatalf("Rückfall auf HTTP: %v", err)
	}
	if c.Secure(ctx) {
		t.Fatal("HTTP als sicher gemeldet")
	}

	SetHTTPSOnly(true)
	if _, err := NewClient(host, port, "", "").BasicInfo(ctx); !errors.Is(err, ErrNoHTTPS) {
		t.Fatalf("Nur HTTPS: %v", err)
	}
	no := false
	if s := (Device{IP: host, Online: true, HTTPS: &no}).Status(); s != "Kein HTTPS" {
		t.Fatalf("Status = %s", s)
	}
}
