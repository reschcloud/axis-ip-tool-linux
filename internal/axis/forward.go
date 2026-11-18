// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Browsers (Firefox, Chrome) can't open a link-local URL with zone
// ("http://[fe80::1%25eth0]/"). The app therefore forwards a local TCP port via the
// correct interface to the device and opens http(s)://127.0.0.1:<port>/.

// Forwarder keeps one local port open per target until Close is called.
type Forwarder struct {
	mu sync.Mutex
	ls map[string]net.Listener // target ("[fe80::1%eth0]:443") → local listener
}

// NewForwarder creates an empty forwarder.
func NewForwarder() *Forwarder { return &Forwarder{ls: map[string]net.Listener{}} }

// Local returns the local address ("127.0.0.1:41234") for host (IPv6 with zone) and port.
func (f *Forwarder) Local(host string, port int) (string, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.ls[target]; ok {
		return l.Addr().String(), nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	f.ls[target] = l
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return // closed
			}
			go pipe(c, target)
		}
	}()
	return l.Addr().String(), nil
}

// Close stops all forwardings (existing connections run out).
func (f *Forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for t, l := range f.ls {
		l.Close()
		delete(f.ls, t)
	}
}

func pipe(c net.Conn, target string) {
	defer c.Close()
	r, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return
	}
	defer r.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(r, c)
	go cp(c, r)
	<-done
	<-done
}
