// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"net"
	"testing"
)

func TestIsRemoteIP(t *testing.T) {
	if remote, known := IsRemoteIP("127.0.0.1"); !known || remote {
		t.Fatalf("loopback: remote=%v known=%v", remote, known)
	}
	if remote, known := IsRemoteIP("fe80::1%eth0"); !known || remote {
		t.Fatalf("link-local: remote=%v known=%v", remote, known)
	}
	if _, known := IsRemoteIP("not-an-ip"); known {
		t.Fatal("hostname reported as known")
	}
	// an address of a local interface must be local
	ifs, err := net.Interfaces()
	if err == nil {
		for _, ifc := range ifs {
			addrs, _ := ifc.Addrs()
			for _, ad := range addrs {
				if n, ok := ad.(*net.IPNet); ok {
					if remote, known := IsRemoteIP(n.IP.String()); !known || remote {
						t.Fatalf("local interface address %s: remote=%v known=%v", n.IP, remote, known)
					}
					break
				}
			}
		}
	}
	// TEST-NET-3 is not attached to a local interface
	if remote, known := IsRemoteIP("203.0.113.1"); !known || !remote {
		t.Fatalf("TEST-NET: remote=%v known=%v", remote, known)
	}
}
