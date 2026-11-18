// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestEUI64LinkLocal(t *testing.T) {
	if got := EUI64LinkLocal("AC:CC:8E:BD:07:90").String(); got != "fe80::aecc:8eff:febd:790" {
		t.Errorf("EUI64 = %s", got)
	}
	if EUI64LinkLocal("kaputt").IsValid() {
		t.Error("invalid MAC yields an address")
	}
}

func TestPickIPv6(t *testing.T) {
	mac := "ACCC8EBD0790"
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"2001:db8::5"}, "2001:db8::5"},
		// link-local takes precedence
		{[]string{"2001:db8::5", "fe80::1234"}, "fe80::1234"},
		// several link-local: the one built from the MAC
		{[]string{"fe80::1", "fe80::aecc:8eff:febd:790", "2001:db8::5"}, "fe80::aecc:8eff:febd:790"},
		// several link-local without EUI-64: the lowest (stable)
		{[]string{"fe80::9", "fe80::2"}, "fe80::2"},
		// zones are removed, IPv4 and garbage ignored
		{[]string{"fe80::aecc:8eff:febd:790%eth0", "10.0.0.1", "x"}, "fe80::aecc:8eff:febd:790"},
	}
	for _, c := range cases {
		if got := PickIPv6(c.in, mac); got != c.want {
			t.Errorf("PickIPv6(%v) = %q, expected %q", c.in, got, c.want)
		}
	}
}

func TestDeviceIPv6(t *testing.T) {
	d := Device{Serial: "ACCC8EBD0790", IP: "10.0.0.5"}
	d.Merge(Device{Serial: "ACCC8EBD0790", IPv6: []string{"2001:db8::5"}, Iface: "eth0", Online: true})
	d.Merge(Device{Serial: "ACCC8EBD0790", IPv6: []string{"fe80::aecc:8eff:febd:790", "2001:db8::5"}, Online: true})
	if len(d.IPv6) != 2 || d.Iface != "eth0" {
		t.Fatalf("Merge: %v %q", d.IPv6, d.Iface)
	}
	if got := d.IPv6Endpoint(); got != "fe80::aecc:8eff:febd:790%eth0" {
		t.Errorf("IPv6Endpoint = %q", got)
	}
}

// Netlink message as sent by the kernel (RTM_NEWNEIGH with NDA_DST and NDA_LLADDR).
func neighMsg(state uint16, addr net.IP, mac []byte) syscall.NetlinkMessage {
	b := make([]byte, 12)
	b[0] = syscall.AF_INET6
	binary.NativeEndian.PutUint32(b[4:8], 1) // lo
	binary.NativeEndian.PutUint16(b[8:10], state)
	attr := func(typ uint16, v []byte) {
		a := make([]byte, 4, 4+len(v)+3)
		binary.NativeEndian.PutUint16(a[0:2], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(a[2:4], typ)
		a = append(a, v...)
		for len(a)%4 != 0 {
			a = append(a, 0)
		}
		b = append(b, a...)
	}
	attr(ndaDst, addr.To16())
	attr(ndaLLAddr, mac)
	return syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: syscall.RTM_NEWNEIGH}, Data: b}
}

func TestParseNeighbors(t *testing.T) {
	mac := []byte{0xac, 0xcc, 0x8e, 0xbd, 0x07, 0x90}
	got := parseNeighbors([]syscall.NetlinkMessage{
		neighMsg(0x02, net.ParseIP("fe80::aecc:8eff:febd:790"), mac), // REACHABLE
		neighMsg(nudFailed, net.ParseIP("fe80::dead"), mac),          // FAILED → weg
		neighMsg(0x04, net.ParseIP("2001:db8::7"), mac),              // STALE
	})
	if len(got) != 2 || got[0].Addr != "fe80::aecc:8eff:febd:790" || got[0].MAC != "ACCC8EBD0790" ||
		got[1].Addr != "2001:db8::7" || got[0].Iface == "" {
		t.Fatalf("parseNeighbors = %+v", got)
	}
}

// Forwarding via ::1 (the zone is handled by net.Dial like with fe80::).
func TestForwarder(t *testing.T) {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "kamera")
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())

	f := NewForwarder()
	defer f.Close()
	p, _ := strconv.Atoi(port)
	local, err := f.Local("::1%lo", p)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := f.Local("::1%lo", p); again != local {
		t.Errorf("zweiter Aufruf: %s statt %s", again, local)
	}
	resp, err := http.Get("http://" + local + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "kamera") {
		t.Errorf("Antwort %q", body)
	}
}
