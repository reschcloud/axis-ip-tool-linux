// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// HTTP digest authentication (RFC 7616) as used by Axis devices.
// The Go standard library has nothing for it.

type challenge struct {
	scheme    string // "digest" or "basic"
	realm     string
	nonce     string
	opaque    string
	algorithm string
	qop       string
	nc        int
}

// parseChallenges reads all WWW-Authenticate headers and prefers digest (SHA-256 before MD5).
func parseChallenges(headers []string) *challenge {
	var best *challenge
	for _, h := range headers {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
		c := &challenge{scheme: strings.ToLower(scheme)}
		for k, v := range parseParams(rest) {
			switch k {
			case "realm":
				c.realm = v
			case "nonce":
				c.nonce = v
			case "opaque":
				c.opaque = v
			case "algorithm":
				c.algorithm = strings.ToUpper(v)
			case "qop":
				c.qop = v
			}
		}
		switch {
		case best == nil:
			best = c
		case c.scheme == "digest" && best.scheme != "digest":
			best = c
		case c.scheme == "digest" && strings.HasPrefix(c.algorithm, "SHA-256") &&
			!strings.HasPrefix(best.algorithm, "SHA-256"):
			best = c
		}
	}
	return best
}

func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				v, s = rest[1:], ""
			} else {
				v, s = rest[1:end+1], rest[end+2:]
			}
		} else {
			v, s, _ = strings.Cut(rest, ",")
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func (c *challenge) authorization(method, uri, user, pass string) string {
	if c.scheme == "basic" {
		return "" // handled by the caller via SetBasicAuth
	}
	var newHash func() hash.Hash = md5.New
	alg := c.algorithm
	if strings.HasPrefix(alg, "SHA-256") {
		newHash = sha256.New
	}
	h := func(s string) string {
		x := newHash()
		x.Write([]byte(s))
		return hex.EncodeToString(x.Sum(nil))
	}
	c.nc++
	nc := fmt.Sprintf("%08x", c.nc)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	cnonce := hex.EncodeToString(b)

	ha1 := h(user + ":" + c.realm + ":" + pass)
	if strings.HasSuffix(alg, "-SESS") {
		ha1 = h(ha1 + ":" + c.nonce + ":" + cnonce)
	}
	ha2 := h(method + ":" + uri)
	qop := ""
	for _, q := range strings.Split(c.qop, ",") {
		if strings.TrimSpace(q) == "auth" {
			qop = "auth"
		}
	}
	var resp string
	if qop != "" {
		resp = h(strings.Join([]string{ha1, c.nonce, nc, cnonce, qop, ha2}, ":"))
	} else {
		resp = h(ha1 + ":" + c.nonce + ":" + ha2)
	}
	parts := []string{
		fmt.Sprintf(`username="%s"`, user),
		fmt.Sprintf(`realm="%s"`, c.realm),
		fmt.Sprintf(`nonce="%s"`, c.nonce),
		fmt.Sprintf(`uri="%s"`, uri),
		fmt.Sprintf(`response="%s"`, resp),
	}
	if alg != "" {
		parts = append(parts, "algorithm="+alg)
	}
	if c.opaque != "" {
		parts = append(parts, fmt.Sprintf(`opaque="%s"`, c.opaque))
	}
	if qop != "" {
		parts = append(parts, "qop="+qop, "nc="+nc, fmt.Sprintf(`cnonce="%s"`, cnonce))
	}
	return "Digest " + strings.Join(parts, ", ")
}
