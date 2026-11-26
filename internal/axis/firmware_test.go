// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func row(icon, href, name, size, sha string) string {
	return fmt.Sprintf(`<tr><td class="icon"><img alt="[%s]"></td><td><a href="%s">%s</a></td>`+
		`<td class="modified">2026-01-12 12:43</td><td class="size">%s</td><td class="sha256">%s</td></tr>`,
		icon, href, name, size, sha)
}

func listing(rows ...string) string {
	return "<html><body><table><tbody>" + row("PARENTDIR", "../", "Parent Directory", "-", "") +
		strings.Join(rows, "\n") + "</tbody></table></body></html>"
}

// fakeAxisCom serves a small firmware tree; the .bin needs the session cookie.
func fakeAxisCom(t *testing.T, bin []byte) (*httptest.Server, *int) {
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])
	downloads := 0
	pages := map[string]string{
		"/ftp/pub_soft/MPQT/": listing(
			row("DIR", "/ftp/pub_soft/MPQT/P3225-VE_Mk_II/", "P3225-VE_Mk_II/", "-", ""),
			row("DIR", "/ftp/pub_soft/MPQT/Q2101-TE/", "Q2101-TE/", "-", "")),
		"/ftp/pub_soft/PACS/": listing(row("DIR", "/ftp/pub_soft/PACS/C1310-E/", "C1310-E/", "-", "")),
		"/ftp/pub_soft/MPQT/Q2101-TE/": listing(
			row("DIR", "x", "11_11_73/", "-", ""), row("DIR", "x", "11_11_238/", "-", ""),
			row("DIR", "x", "12_9_57/", "-", ""), row("DIR", "x", "12_11_77/", "-", ""),
			row("DIR", "x", "10_12_212_TTA/", "-", ""), row("DIR", "x", "latest/", "-", "")),
		"/ftp/pub_soft/MPQT/Q2101-TE/12_11_77/": listing(
			row("FILE", "/ftp/pub_soft/MPQT/Q2101-TE/12_11_77/Q2101-TE_12_11_77.bin", "Q2101-TE_12_11_77.bin", "2.0M", sha),
			row("FILE", "x", "Q2101-TE_12_11_77_licenses.txt", "999.8K", sha)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".bin") {
			if !strings.Contains(r.Header.Get("Cookie"), MyAxisCookie+"=good") {
				http.Redirect(w, r, "/my-axis/login", http.StatusFound)
				return
			}
			if r.Method == http.MethodGet {
				downloads++
			}
			w.Write(bin)
			return
		}
		if p, ok := pages[r.URL.Path]; ok {
			w.Write([]byte(p))
			return
		}
		http.Redirect(w, r, "/page-not-found", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	old := AxisSite
	AxisSite = srv.URL
	t.Cleanup(func() { AxisSite = old })
	return srv, &downloads
}

func TestFirmwareCatalog(t *testing.T) {
	ctx := context.Background()
	bin := bytes.Repeat([]byte("AXIS"), 1<<19) // 2 MiB
	_, downloads := fakeAxisCom(t, bin)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cat := NewFirmwareCatalog()

	p, ok, err := cat.FindProduct(ctx, "", "AXIS Q2101-TE Thermal Camera")
	if err != nil || !ok || p != (FirmwareProduct{"MPQT", "Q2101-TE"}) {
		t.Fatalf("prefix match: %+v %v %v", p, ok, err)
	}
	if p, ok, _ := cat.FindProduct(ctx, "C1310-E"); !ok || p.Source != "PACS" {
		t.Fatalf("PACS: %+v %v", p, ok)
	}
	if _, ok, _ := cat.FindProduct(ctx, "M1065-L"); ok {
		t.Fatal("unknown model found")
	}
	vs, err := cat.Versions(ctx, FirmwareProduct{"MPQT", "Q2101-TE"})
	if err != nil || len(vs) != 4 || vs[0].String() != "12.11.77" || vs[3].String() != "11.11.73" {
		t.Fatalf("versions = %v, %v", vs, err)
	}
	cur, _ := ParseFWVersion("11.11.73")
	ch := FirmwareChoices(cur, vs)
	if len(ch) != 2 || ch[0].Version.String() != "11.11.238" || ch[0].Label != "Current track" ||
		ch[1].Version.String() != "12.11.77" || ch[1].Label != "Latest LTS · Latest" {
		t.Fatalf("choices = %+v", ch)
	}
	if cur, _ := ParseFWVersion("12.11.77"); len(FirmwareChoices(cur, vs)) != 0 {
		t.Fatal("up to date device got choices")
	}

	f, err := cat.File(ctx, p, vs[0])
	if err != nil || f.Name != "Q2101-TE_12_11_77.bin" || f.Size != 2<<20 {
		t.Fatalf("file = %+v, %v", f, err)
	}
	if _, err := cat.Download(ctx, f, "bad", nil); !errors.Is(err, ErrMyAxisLogin) {
		t.Fatalf("without session: %v", err)
	}
	if err := cat.CheckMyAxisSession(ctx, "bad"); !errors.Is(err, ErrMyAxisLogin) {
		t.Fatalf("check bad session: %v", err)
	}
	var last int64
	path, err := cat.Download(ctx, f, "good", func(done, total int64) { last = done })
	if err != nil || last != int64(len(bin)) {
		t.Fatalf("download: %v (progress %d)", err, last)
	}
	if path2, err := cat.Download(ctx, f, "", nil); err != nil || path2 != path || *downloads != 1 {
		t.Fatalf("cache: %s %v, %d downloads", path2, err, *downloads)
	}
	f.SHA256 = strings.Repeat("0", 64)
	if _, err := cat.Download(ctx, f, "good", nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("checksum mismatch: %v", err)
	}
}

func TestMyAxisSessionStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AXIS_MYAXIS_SESSION", "")
	if _, ok := LoadMyAxisSession(); ok {
		t.Fatal("session without file")
	}
	if err := SaveMyAxisSession(MyAxisSession{Value: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if s, ok := LoadMyAxisSession(); !ok || s.Value != "abc123" {
		t.Fatalf("load = %+v %v", s, ok)
	}
	if fi, _ := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ip-tool-for-axis", "myaxis-session.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	t.Setenv("AXIS_MYAXIS_SESSION", MyAxisCookie+"=fromenv")
	if s, _ := LoadMyAxisSession(); s.Value != "fromenv" {
		t.Fatalf("env = %+v", s)
	}
	t.Setenv("AXIS_MYAXIS_SESSION", "")
	ForgetMyAxisSession()
	if _, ok := LoadMyAxisSession(); ok {
		t.Fatal("session after forget")
	}
}

func TestUpgradeFirmware(t *testing.T) {
	ctx := context.Background()
	f, c := loggedIn(t)
	path := filepath.Join(t.TempDir(), "Q2101-TE_12_11_77.bin")
	os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 3<<20), 0o644)

	var last, total int64
	v, err := c.UpgradeFirmware(ctx, path, FactoryDefaultSoft, func(d, t int64) { last, total = d, t })
	if err != nil || v != "12.11.77" {
		t.Fatalf("upgrade: %q %v", v, err)
	}
	if last != total || total < 3<<20 {
		t.Fatalf("progress %d/%d", last, total)
	}
	if len(f.fwUploads) != 1 {
		t.Fatalf("%d uploads (digest should be primed by status)", len(f.fwUploads))
	}
	// parse the multipart body like the device
	body := f.fwUploads[0]
	boundary := ""
	for _, line := range strings.SplitN(string(body[:200]), "\r\n", 2)[:1] {
		boundary = strings.TrimPrefix(line, "--")
	}
	_, params, _ := mime.ParseMediaType("multipart/form-data; boundary=" + boundary)
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "json" {
		t.Fatalf("json part: %v", err)
	}
	var js bytes.Buffer
	js.ReadFrom(part)
	if !strings.Contains(js.String(), `"method":"upgrade"`) || !strings.Contains(js.String(), `"factoryDefaultMode":"soft"`) {
		t.Fatalf("json = %s", js.String())
	}
	part, err = mr.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() != "Q2101-TE_12_11_77.bin" {
		t.Fatalf("file part: %v", err)
	}
	var fb bytes.Buffer
	fb.ReadFrom(part)
	if fb.Len() != 3<<20 {
		t.Fatalf("file size %d", fb.Len())
	}

	small := filepath.Join(t.TempDir(), "x.bin")
	os.WriteFile(small, []byte("tiny"), 0o644)
	if _, err := c.UpgradeFirmware(ctx, small, "", nil); err == nil {
		t.Fatal("tiny file accepted")
	}
}
