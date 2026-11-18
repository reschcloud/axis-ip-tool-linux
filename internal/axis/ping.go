// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// Ping sends an ICMP echo to addr (IPv4, IPv6, link-local with zone "fe80::1%eth0") and
// returns the round-trip time. Uses unprivileged ICMP sockets (net.ipv4.ping_group_range,
// also applies to IPv6); the kernel assigns the identifier and delivers only matching replies.
func Ping(addr string, timeout time.Duration) (time.Duration, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return 0, err
	}
	family, proto, reqType, replyType := syscall.AF_INET, syscall.IPPROTO_ICMP, byte(8), byte(0)
	if a.Is6() && !a.Is4In6() {
		family, proto, reqType, replyType = syscall.AF_INET6, syscall.IPPROTO_ICMPV6, 128, 129
	}
	fd, err := syscall.Socket(family, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, proto)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), "icmp")
	pc, err := net.FilePacketConn(f) // duplicates fd
	f.Close()
	if err != nil {
		return 0, err
	}
	defer pc.Close()

	seq := uint16(pingSeq.Add(1))
	msg := []byte{reqType, 0, 0, 0, 0, 0, byte(seq >> 8), byte(seq), 'a', 'x', 'i', 's'}
	dst := &net.UDPAddr{IP: a.AsSlice(), Zone: a.Zone()}
	start := time.Now()
	pc.SetDeadline(start.Add(timeout))
	if _, err := pc.WriteTo(msg, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		if n >= 8 && buf[0] == replyType && uint16(buf[6])<<8|uint16(buf[7]) == seq {
			return time.Since(start), nil
		}
	}
}

var pingSeq atomic.Uint32

// IsTimeout: err means "no reply" (and not, say, "ping not permitted").
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
