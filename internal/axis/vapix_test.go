// SPDX-License-Identifier: AGPL-3.0-or-later

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

// fakeCamera simulates the required VAPIX endpoints incl. digest auth.
type fakeCamera struct {
	mu                 sync.Mutex
	users              map[string]string
	calls              []string
	jsonBody           []map[string]any
	locked             bool // basicdeviceinfo only with login (AXIS OS 12 after setup)
	legacy             bool // old firmware: no systemready.cgi, pwdgrp.cgi open in factory state
	hostname           string
	ignoreJSONHostname bool // like some AXIS OS 9: setHostnameConfiguration has no effect
	ntp                map[string]any
	params             map[string]string // ImageSource./System. parameters (param.cgi)
	ptz                bool              // PTZ camera (ptz.cgi)
	ptzQueries         []string          // raw queries sent to ptz.cgi
	fwUploads          [][]byte          // multipart bodies sent to firmwaremanagement.cgi
	opticsSetup        bool              // lens via opticssetup.cgi (no opticscontrol.cgi)
	opticsSetupQueries []string
	optics             []map[string]any // requests to opticscontrol.cgi
	frames             int              // number of MJPEG frames before the stream ends
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
	if r.URL.Path == "/axis-cgi/systemready.cgi" && !f.legacy {
		// like P3225-VE Mk II: without "context" only "Invalid POST data"
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &req) != nil || req["context"] == nil {
			io.WriteString(w, `{"apiVersion":"1.1","error":{"code":1000,"message":"Invalid POST data"}}`)
			return
		}
		needs := "no"
		if len(f.users) == 0 {
			needs = "yes"
		}
		io.WriteString(w, `{"apiVersion":"1.4","method":"systemready","data":{"systemready":"yes","needsetup":"`+needs+`"}}`)
		return
	}
	// AXIS OS 11: pwdgrp.cgi?action=get requires a login even in factory state,
	// creating via POST works without.
	pwdgrpOpen := r.URL.Path == "/axis-cgi/pwdgrp.cgi" && len(f.users) == 0 && (f.legacy || r.Method == http.MethodPost)
	open := (r.URL.Path == "/axis-cgi/basicdeviceinfo.cgi" && !f.locked) || pwdgrpOpen
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
		if req["method"] == "setHostnameConfiguration" && !f.ignoreJSONHostname {
			f.hostname = req["params"].(map[string]any)["staticHostname"].(string)
		}
		if req["method"] == "getNetworkInfo" {
			io.WriteString(w, `{"data":{"system":{"hostname":{"useDhcpHostname":false,"staticHostname":"`+f.hostname+`"},
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
	case "/axis-cgi/factorydefault.cgi", "/axis-cgi/hardfactorydefault.cgi":
		f.users = map[string]string{}
		io.WriteString(w, "OK")
	case "/axis-cgi/param.cgi":
		q := r.URL.Query() // query and POST form
		if form, err := url.ParseQuery(string(body)); err == nil {
			for k, v := range form {
				q[k] = v
			}
		}
		if q.Get("action") == "update" {
			if h := q.Get("Network.HostName"); h != "" {
				f.hostname = h
			}
			for k, v := range q {
				if strings.HasPrefix(k, "ImageSource.") || strings.HasPrefix(k, "System.") {
					if _, ok := f.params[k]; !ok {
						io.WriteString(w, "# Error: Error setting '"+k+"' to '"+v[0]+"'!")
						return
					}
					f.params[k] = v[0]
				}
			}
			io.WriteString(w, "OK")
			return
		}
		if q.Get("group") == "Properties.PTZ" {
			io.WriteString(w, "root.Properties.PTZ.PTZ="+yesNo(f.ptz)+"\n")
			return
		}
		if q.Get("group") == "PTZ.Support" {
			for _, k := range []string{"ContinuousPan", "ContinuousTilt", "ContinuousZoom", "AreaZoom", "ServerPreset"} {
				io.WriteString(w, "root.PTZ.Support.S1."+k+"=true\n")
			}
			io.WriteString(w, "root.PTZ.Support.S1.ContinuousFocus=false\n")
			return
		}
		if q.Get("group") == "ImageSource" {
			for k, v := range f.params {
				if strings.HasPrefix(k, "ImageSource.") {
					io.WriteString(w, "root."+k+"="+v+"\n")
				}
			}
			return
		}
		io.WriteString(w, "root.Brand.Brand=AXIS\n")
	case "/axis-cgi/firmwaremanagement.cgi":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			f.fwUploads = append(f.fwUploads, body)
			io.WriteString(w, `{"apiVersion":"1.0","method":"upgrade","data":{"firmwareVersion":"12.11.77"}}`)
			return
		}
		io.WriteString(w, `{"apiVersion":"1.0","method":"status","data":{"activeFirmwareVersion":"11.11.73","isCommited":true}}`)
	case "/axis-cgi/com/ptz.cgi":
		f.ptzQueries = append(f.ptzQueries, r.URL.RawQuery)
		if r.URL.Query().Get("query") == "position" {
			if f.ptz {
				io.WriteString(w, "pan=0\ntilt=0\nzoom=1\n")
			}
			return
		}
		if r.URL.Query().Get("query") == "presetposcam" {
			io.WriteString(w, "Preset Positions for camera 1\npresetposno3=Door\npresetposno1=Home\n")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "/axis-cgi/opticssetup.cgi":
		if !f.opticsSetup {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.opticsSetupQueries = append(f.opticsSetupQueries, r.URL.RawQuery)
		if r.URL.Query().Get("monitor") == "poll" {
			io.WriteString(w, `<?xml version="1.0"?><devInfo><opticsSetupState focusMeasure="2.5e7" `+
				`focusOperationInProgress="1" focusPosition="0.281664" isDay="1" `+
				`zoomOperationInProgress="0" zoomPosition="0.150000" /></devInfo>`)
			return
		}
		io.WriteString(w, "ok\n")
	case "/axis-cgi/opticscontrol.cgi":
		if f.opticsSetup {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req map[string]any
		json.Unmarshal(body, &req)
		f.optics = append(f.optics, req)
		switch req["method"] {
		case "getCapabilities":
			io.WriteString(w, `{"apiVersion":"1.2","method":"getCapabilities","data":{"numberOfOptics":1,
			  "optics":[{"opticsId":"0","capabilities":["focus","zoom","calibrateFocus","calibrateZoom"],"maxMagnification":3.5}]}}`)
		case "getOptics":
			io.WriteString(w, `{"data":{"numberOfOptics":1,"optics":[{"opticsId":"0","focusPosition":0.25,
			  "focusMoving":true,"magnification":2.0,"zoomMoving":false}]}}`)
		case "setFocus":
			io.WriteString(w, `{"error":{"code":2105,"message":"Authorization failed"}}`)
		default:
			io.WriteString(w, `{"data":{}}`)
		}
	case "/axis-cgi/mjpg/video.cgi":
		if r.URL.Query().Get("resolution") != "" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=myboundary")
		for i := 0; i < f.frames; i++ {
			jpg := []byte{0xFF, 0xD8, byte(i), 0xFF, 0xD9}
			io.WriteString(w, "--myboundary\r\nContent-Type: image/jpeg\r\nContent-Length: "+
				strconv.Itoa(len(jpg))+"\r\n\r\n")
			w.Write(jpg)
			io.WriteString(w, "\r\n")
		}
	case "/axis-cgi/time.cgi":
		io.WriteString(w, `{"error":{"code":2002,"message":"Invalid time zone"}}`)
	default:
		w.WriteHeader(404)
	}
}

func startFake(t *testing.T) (*fakeCamera, string, int) {
	f := &fakeCamera{users: map[string]string{}, hostname: "cam1", params: map[string]string{
		// like P3225-VE Mk II (firmware 9.80) in factory state
		"ImageSource.I0.CaptureFrequency":      "60Hz",
		"ImageSource.I0.CameraTiltOrientation": "0",
		"ImageSource.I0.Sensor.CaptureMode":    "1",
		"System.CaptureFrequencySet":           "no",
	}}
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
		t.Fatal("NeedsInitialUser after setup = true")
	}

	if err := NewClient(host, port, "root", "wrong").CheckLogin(ctx); err != ErrAuth {
		t.Fatalf("wrong password: %v", err)
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
		t.Fatalf("address not applied: %v", addrs)
	}

	if err := c.SetTime(ctx, "Mars/Olympus", false); err == nil || !strings.Contains(err.Error(), "Invalid time zone") {
		t.Fatalf("API error not reported: %v", err)
	}
}

func TestLockedInfoNTPSnapshot(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)
	f.users["root"] = "geheim123"
	f.locked = true

	if _, err := NewClient(host, port, "", "").BasicInfo(ctx); !errors.Is(err, ErrAuth) {
		t.Fatalf("locked anonymously: %v", err)
	}
	c := NewClient(host, port, "root", "geheim123")
	info, err := c.BasicInfo(ctx)
	if err != nil || info.Firmware != "11.11.73" {
		t.Fatalf("BasicInfo with login = %+v, %v", info, err)
	}

	n, err := c.GetNTP(ctx)
	if err != nil || !n.Enabled || n.FromDHCP || len(n.Servers) != 1 || n.Synced == nil || !*n.Synced {
		t.Fatalf("GetNTP = %+v, %v", n, err)
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true}); err == nil {
		t.Fatal("NTP without server accepted")
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true, Servers: []string{"0.at.pool.ntp.org", "10.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	if f.ntp["serversSource"] != "static" || len(f.ntp["staticServers"].([]any)) != 2 {
		t.Fatalf("SetNTP sent %v", f.ntp)
	}
	if err := c.SetNTP(ctx, NTPConfig{Enabled: true, FromDHCP: true}); err != nil || f.ntp["serversSource"] != "DHCP" {
		t.Fatalf("SetNTP DHCP: %v %v", f.ntp, err)
	}

	img, err := c.Snapshot(ctx, 640)
	if err != nil || !isJPEG(img) {
		t.Fatalf("Snapshot = %v, %v", img, err)
	}
	if _, err := NewClient(host, port, "root", "wrong").Snapshot(ctx, 640); !errors.Is(err, ErrAuth) {
		t.Fatalf("snapshot wrong password: %v", err)
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
			t.Errorf("expected error for %+v", c)
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
		t.Error("invalid mask accepted")
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
	srv := httptest.NewTLSServer(f) // self-signed, like the cameras
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	p, _ := strconv.Atoi(port)
	resetScheme(t, p)

	// HTTP port points nowhere – only HTTPS may be used
	c := NewClient(host, 1, "root", "geheim123")
	info, err := c.BasicInfo(ctx)
	if err != nil || info.Firmware != "11.11.73" {
		t.Fatalf("BasicInfo via HTTPS = %+v, %v", info, err)
	}
	if !c.Secure(ctx) {
		t.Fatal("client reports no HTTPS connection")
	}
	if d := (Device{IP: host, HTTPS: &[]bool{true}[0]}); !strings.HasPrefix(d.WebURL(), "https://") {
		t.Fatal(d.WebURL())
	}
}

func TestHTTPFallbackAndHTTPSOnly(t *testing.T) {
	ctx := context.Background()
	_, host, port := startFake(t) // HTTP only
	// port for HTTPS: a port that is certainly closed
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	resetScheme(t, closed)

	c := NewClient(host, port, "", "")
	if _, err := c.BasicInfo(ctx); err != nil {
		t.Fatalf("fallback to HTTP: %v", err)
	}
	if c.Secure(ctx) {
		t.Fatal("HTTP reported as secure")
	}

	SetHTTPSOnly(true)
	if _, err := NewClient(host, port, "", "").BasicInfo(ctx); !errors.Is(err, ErrNoHTTPS) {
		t.Fatalf("Nur HTTPS: %v", err)
	}
	no := false
	if s := (Device{IP: host, Online: true, HTTPS: &no}).Status(); s != StatusNoHTTPS {
		t.Fatalf("Status = %s", s)
	}
}

// Factory state with AXIS OS 11 (systemready) and old firmware (pwdgrp heuristic).
func TestNeedsInitialUser(t *testing.T) {
	ctx := context.Background()
	for _, legacy := range []bool{false, true} {
		f, host, port := startFake(t)
		f.legacy = legacy
		anon := NewClient(host, port, "", "")
		if needs, err := anon.NeedsInitialUser(ctx); err != nil || !needs {
			t.Fatalf("legacy=%v: factory state not detected: %v, %v", legacy, needs, err)
		}
		if err := NewClient(host, port, "root", "whatever1").CheckLogin(ctx); !errors.Is(err, ErrNoUser) {
			t.Fatalf("legacy=%v: CheckLogin in factory state = %v", legacy, err)
		}
		if err := anon.CreateInitialUser(ctx, "admin", "geheim123"); err != nil {
			t.Fatalf("legacy=%v: %v", legacy, err)
		}
		// old firmware: 401 is ambiguous (newer firmware locks in factory state too)
		if needs, err := anon.NeedsInitialUser(ctx); needs || (err != nil && !(legacy && errors.Is(err, ErrSetupUnknown))) {
			t.Fatalf("legacy=%v: after setup = %v, %v", legacy, needs, err)
		}
		if err := anon.CreateInitialUser(ctx, "admin2", "geheim123"); !errors.Is(err, ErrHasUser) {
			t.Fatalf("legacy=%v: second initial password = %v", legacy, err)
		}
		if err := NewClient(host, port, "admin", "wrong").CheckLogin(ctx); err != ErrAuth {
			t.Fatalf("legacy=%v: wrong password = %v", legacy, err)
		}
		if err := NewClient(host, port, "admin", "geheim123").CheckLogin(ctx); err != nil {
			t.Fatalf("legacy=%v: correct password = %v", legacy, err)
		}
	}
}

func TestFactoryReset(t *testing.T) {
	ctx := context.Background()
	_, host, port := startFake(t)
	anon := NewClient(host, port, "", "")
	if err := anon.CreateInitialUser(ctx, "root", "geheim123"); err != nil {
		t.Fatal(err)
	}
	if err := NewClient(host, port, "root", "wrong").FactoryReset(ctx, true); !errors.Is(err, ErrAuth) {
		t.Fatalf("reset with wrong password = %v", err)
	}
	if err := NewClient(host, port, "root", "geheim123").FactoryReset(ctx, true); err != nil {
		t.Fatal(err)
	}
	if needs, err := anon.NeedsInitialUser(ctx); err != nil || !needs {
		t.Fatalf("no factory state after reset: %v, %v", needs, err)
	}
}

// AXIS OS 9: hostname via network_settings.cgi has no effect → fallback to param.cgi.
func TestHostnameFallback(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)
	f.users["root"] = "geheim123"
	f.ignoreJSONHostname = true
	c := NewClient(host, port, "root", "geheim123")
	cfg, err := c.GetNetwork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hostname = "camera-09"
	if err := c.SetNetwork(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if f.hostname != "camera-09" {
		t.Fatalf("Hostname = %q", f.hostname)
	}
}

// Power line frequency/mounting position at commissioning; missing parameters (e.g. a
// thermal camera without power line frequency) are skipped.
func TestApplyInitialSetup(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)
	f.users["root"] = "geheim123"
	c := NewClient(host, port, "root", "geheim123")
	skipped, err := c.ApplyInitialSetup(ctx, InitialSetup{PowerFrequency: 50, Mount: MountCeiling})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("ApplyInitialSetup = %v, %v", skipped, err)
	}
	want := map[string]string{"ImageSource.I0.CaptureFrequency": "50Hz",
		"ImageSource.I0.CameraTiltOrientation": "-90", "System.CaptureFrequencySet": "yes"}
	for k, v := range want {
		if f.params[k] != v {
			t.Errorf("%s = %q, expected %q", k, f.params[k], v)
		}
	}

	delete(f.params, "ImageSource.I0.CaptureFrequency")
	f.params["System.CaptureFrequencySet"] = "no"
	skipped, err = c.ApplyInitialSetup(ctx, InitialSetup{PowerFrequency: 60, Mount: MountWall})
	if err != nil || len(skipped) != 1 || skipped[0] != SettingPowerFrequency {
		t.Fatalf("without power line frequency: %v, %v", skipped, err)
	}
	if f.params["ImageSource.I0.CameraTiltOrientation"] != "0" || f.params["System.CaptureFrequencySet"] != "no" {
		t.Errorf("without power line frequency: %v", f.params)
	}
	if _, err := c.ApplyInitialSetup(ctx, InitialSetup{PowerFrequency: 55}); err == nil {
		t.Error("55 Hz accepted")
	}
	if _, err := NewClient(host, port, "root", "wrong").ApplyInitialSetup(ctx, InitialSetup{Mount: MountDesk}); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong password = %v", err)
	}
}
