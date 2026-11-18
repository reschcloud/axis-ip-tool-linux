// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// IPv6 of the cameras: Axis devices often announce only IPv4 via mDNS. The addresses
// therefore also come from the kernel's neighbor table after all nodes on the link have
// been addressed via multicast ping (ff02::1). Matched by MAC (= serial number).

// EUI64LinkLocal builds the link-local address from a MAC (12 hex characters).
func EUI64LinkLocal(mac string) netip.Addr {
	b, err := hex.DecodeString(NormMAC(mac))
	if err != nil || len(b) != 6 {
		return netip.Addr{}
	}
	var a [16]byte
	a[0], a[1] = 0xfe, 0x80
	copy(a[8:], []byte{b[0] ^ 0x02, b[1], b[2], 0xff, 0xfe, b[3], b[4], b[5]})
	return netip.AddrFrom16(a)
}

// PickIPv6 chooses the address to display: link-local before all others; with several
// link-local addresses the one built from the MAC (EUI-64), otherwise the lowest (stable).
func PickIPv6(addrs []string, mac string) string {
	var ll, other []netip.Addr
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is6() || a.Is4In6() {
			continue
		}
		a = a.WithZone("")
		if a.IsLinkLocalUnicast() {
			ll = append(ll, a)
		} else if a.IsGlobalUnicast() {
			other = append(other, a)
		}
	}
	pick := func(list []netip.Addr) string {
		if len(list) == 0 {
			return ""
		}
		if eui := EUI64LinkLocal(mac); eui.IsValid() && slices.Contains(list, eui) {
			return eui.String()
		}
		return slices.MinFunc(list, func(a, b netip.Addr) int { return a.Compare(b) }).String()
	}
	if s := pick(ll); s != "" {
		return s
	}
	return pick(other)
}

// IsIPv6LinkLocal: fe80::/10.
func IsIPv6LinkLocal(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6() && a.IsLinkLocalUnicast()
}

// IfaceName returns the name for an interface index ("" if unknown).
func IfaceName(index int) string {
	if index <= 0 {
		return ""
	}
	if ifc, err := net.InterfaceByIndex(index); err == nil {
		return ifc.Name
	}
	return ""
}

// IfaceForIPv4 finds the interface whose IPv4 network contains ip (for SSDP replies).
func IfaceForIPv4(ip string) string {
	a := net.ParseIP(ip)
	if a == nil || a.To4() == nil {
		return ""
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok && n.IP.To4() != nil && n.Contains(a) {
				return ifc.Name
			}
		}
	}
	return ""
}

// Neighbor6 is an entry of the IPv6 neighbor table.
type Neighbor6 struct {
	Addr  string
	MAC   string // 12 upper-case hex characters
	Iface string
}

const (
	ndaDst    = 1
	ndaLLAddr = 2
	nudFailed = 0x20
	nudNoARP  = 0x40
)

// Neighbors6 reads the IPv6 neighbor table via netlink (no root privileges needed).
func Neighbors6() ([]Neighbor6, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_INET6)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil, err
	}
	return parseNeighbors(msgs), nil
}

func parseNeighbors(msgs []syscall.NetlinkMessage) []Neighbor6 {
	var out []Neighbor6
	for _, m := range msgs {
		// struct ndmsg: family u8, pad u8, pad u16, ifindex i32, state u16, flags u8, type u8
		if m.Header.Type != syscall.RTM_NEWNEIGH || len(m.Data) < 12 {
			continue
		}
		ifindex := int(int32(binary.NativeEndian.Uint32(m.Data[4:8])))
		state := binary.NativeEndian.Uint16(m.Data[8:10])
		if state&(nudFailed|nudNoARP) != 0 {
			continue
		}
		var n Neighbor6
		for b := m.Data[12:]; len(b) >= 4; {
			l := int(binary.NativeEndian.Uint16(b[0:2]))
			typ := binary.NativeEndian.Uint16(b[2:4])
			if l < 4 || l > len(b) {
				break
			}
			v := b[4:l]
			switch typ {
			case ndaDst:
				if a, ok := netip.AddrFromSlice(v); ok && a.Is6() {
					n.Addr = a.String()
				}
			case ndaLLAddr:
				if len(v) == 6 {
					n.MAC = strings.ToUpper(hex.EncodeToString(v))
				}
			}
			b = b[(l+3)&^3:] // attributes are 4-byte aligned
			if len(b) == 0 {
				break
			}
		}
		if n.Addr != "" && n.MAC != "" {
			n.Iface = IfaceName(ifindex)
			out = append(out, n)
		}
	}
	return out
}

// PingAllNodes sends an ICMPv6 echo to ff02::1 on the interface; the replies fill the
// neighbor table. Uses an unprivileged ICMP socket (net.ipv4.ping_group_range).
func PingAllNodes(iface string) error {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMPV6)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	// echo request: type 128, code 0; the kernel sets checksum and ID
	msg := []byte{128, 0, 0, 0, 0, 0, 0, 1}
	dst := &syscall.SockaddrInet6{ZoneId: uint32(ifc.Index)}
	copy(dst.Addr[:], net.ParseIP("ff02::1").To16())
	for i := 0; i < 2; i++ {
		if err := syscall.Sendto(fd, msg, 0, dst); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
