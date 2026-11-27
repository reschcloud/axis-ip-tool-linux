// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"net"
	"net/netip"
)

// IsRemoteIP reports whether ip is outside the directly attached networks, i.e. traffic
// has to pass a router. known is false if the address or the local networks cannot be
// determined (e.g. a hostname that has not been resolved yet).
func IsRemoteIP(ip string) (remote, known bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.IsValid() {
		return false, false
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false, true
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return false, false
	}
	comparable := false
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, ad := range addrs {
			n, ok := ad.(*net.IPNet)
			if !ok {
				continue
			}
			b, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			b = b.Unmap()
			if b.Is4() != a.Is4() {
				continue
			}
			ones, bits := n.Mask.Size()
			if bits == 0 {
				continue
			}
			comparable = true
			if netip.PrefixFrom(b, ones).Contains(a) {
				return false, true
			}
		}
	}
	if !comparable {
		return false, false
	}
	return true, true
}
