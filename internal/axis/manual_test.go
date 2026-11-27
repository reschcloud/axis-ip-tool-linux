// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func parse(t *testing.T, input string) []ManualEntry {
	t.Helper()
	entries, err := ParseManual(input)
	if err != nil {
		t.Fatalf("ParseManual(%q): %v", input, err)
	}
	return entries
}

func allDevices(entries []ManualEntry) []Device {
	var out []Device
	for _, e := range entries {
		out = append(out, e.Devices...)
	}
	return out
}

// The same entry repeated in one input is only added once.
func TestParseManualDedup(t *testing.T) {
	entries := parse(t, "10.0.0.1, 10.0.0.1; 10.0.0.1")
	if len(entries) != 1 {
		t.Fatalf("%d entries: %+v", len(entries), entries)
	}
}

func TestParseManualSeparators(t *testing.T) {
	entries := parse(t, " 10.0.0.1,10.0.0.2; 10.0.0.3\n10.0.0.4\t10.0.0.5 ;, 10.0.0.6 ")
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}
	if len(entries) != len(want) {
		t.Fatalf("%d entries: %+v", len(entries), entries)
	}
	for i, e := range entries {
		if len(e.Devices) != 1 || e.Devices[0].IP != want[i] {
			t.Fatalf("entry %d: %+v", i, e)
		}
	}
}

func TestParseManualHostnameAndURL(t *testing.T) {
	tests := []struct {
		in       string
		ip       string
		hostname string
		scheme   string
		port     int
		https    int
	}{
		{"camera.local", "camera.local", "camera.local", "", 80, 0},
		{"axis-accc8e123456", "axis-accc8e123456", "axis-accc8e123456", "", 80, 0},
		{"face", "face", "face", "", 80, 0}, // not mistaken for an IP
		{"http://camera.local:8080/path", "camera.local", "camera.local", "http", 8080, 0},
		{"https://cam.example.com/", "cam.example.com", "cam.example.com", "https", 80, 0},
		{"https://10.0.0.5:8443/", "10.0.0.5", "", "https", 80, 8443},
		{"http://[2001:db8::1]:8080/", "2001:db8::1", "", "http", 8080, 0},
		{"http://[fe80::1%25eth0]/", "fe80::1%eth0", "", "http", 80, 0},
	}
	for _, tt := range tests {
		dev := parse(t, tt.in)[0].Devices[0]
		if dev.IP != tt.ip || dev.Hostname != tt.hostname || dev.Scheme != tt.scheme ||
			dev.Port != tt.port || dev.HTTPSPort != tt.https {
			t.Errorf("%q: %+v", tt.in, dev)
		}
	}
}

func TestParseManualRanges(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"192.168.0.0/30", []string{"192.168.0.1", "192.168.0.2"}},
		{"192.168.0.0/31", []string{"192.168.0.0", "192.168.0.1"}},
		{"192.168.0.5/32", []string{"192.168.0.5"}},
		{"10.0.0.5-10.0.0.7", []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}},
		{"10.0.0.250-252", []string{"10.0.0.250", "10.0.0.251", "10.0.0.252"}},
		{"10.0.0.5..7", []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}},
		{"10.0.0.5 - 7", []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}},
		{"10.0.0.5 .. 7", []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}},
		{"10.0.0.5–7", []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}}, // en dash
		{"2001:db8::1-2001:db8::3", []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}},
	}
	for _, tt := range tests {
		devs := allDevices(parse(t, tt.in))
		var got []string
		for _, d := range devs {
			got = append(got, d.IP)
		}
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%q: %v, expected %v", tt.in, got, tt.want)
		}
	}
}

// A spaced hyphen between hostnames must not be swallowed into one hostname.
func TestParseManualSpacedHyphen(t *testing.T) {
	entries := parse(t, "camera1 - camera2")
	if len(entries) != 2 {
		t.Fatalf("%d entries: %+v", len(entries), entries)
	}
	if entries[0].Devices[0].IP != "camera1" || entries[1].Devices[0].IP != "camera2" {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestParseManualWildcard(t *testing.T) {
	devs := allDevices(parse(t, "192.168.0.*"))
	if len(devs) != 254 || devs[0].IP != "192.168.0.1" || devs[len(devs)-1].IP != "192.168.0.254" {
		t.Fatalf("%d devices, first %q, last %q", len(devs), devs[0].IP, devs[len(devs)-1].IP)
	}
}

func TestParseManualErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"192.168.0.256",
		"fe80::1",
		"bad_host!",
		"10.0.0.7-10.0.0.5",
		"10.0.0.5-2001:db8::5",
		"10.0.0.0/8",  // too many addresses
		"10.0.0.5/24", // host bits set
		"255.255.255.255",
		"127.0.0.1", // loopback
		"::1",
		"192.168.*.*", // wildcard only in the last octet
		"ftp://camera/",
		"http://",
	} {
		if _, err := ParseManual(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

// A whole input is rejected before anything is monitored if one entry is invalid.
func TestParseManualAtomic(t *testing.T) {
	if _, err := ParseManual("10.0.0.1, broken!"); err == nil {
		t.Fatal("expected error")
	}
}

// A manual URL's scheme and port override the protocol detection.
func TestDeviceClientFromURL(t *testing.T) {
	ctx := context.Background()

	// http:// on a custom HTTP port
	_, host, port := startFake(t)
	d := Device{IP: host, Port: port, Scheme: "http"}
	if info, err := d.Client("", "").BasicInfo(ctx); err != nil || info.Model == "" {
		t.Fatalf("HTTP client: %+v, %v", info, err)
	}

	// https:// on a custom HTTPS port; the HTTP port points nowhere
	srv := httptest.NewTLSServer(&fakeCamera{})
	t.Cleanup(srv.Close)
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	n, _ := strconv.Atoi(p)
	d = Device{IP: h, Port: 1, Scheme: "https", HTTPSPort: n}
	c := d.Client("", "")
	if info, err := c.BasicInfo(ctx); err != nil || info.Model == "" {
		t.Fatalf("HTTPS client: %+v, %v", info, err)
	}
	if !c.Secure(ctx) {
		t.Fatal("HTTPS client reports HTTP")
	}
	if got := d.WebURL(); got != "https://"+net.JoinHostPort(h, p)+"/" {
		t.Fatalf("WebURL = %q", got)
	}
}
