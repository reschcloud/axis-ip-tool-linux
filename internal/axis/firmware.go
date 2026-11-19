// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AXIS OS catalog on axis.com (like OADM): the public directory listings under
// /ftp/pub_soft/<MPQT|PACS>/<product>/<version>/ contain file names, sizes and SHA-256.
// Listings are public; downloading a .bin needs the My Axis session cookie
// (axis_connect_session_sid), otherwise axis.com redirects to the login page.

// AxisSite is the base URL of axis.com (overridable in tests).
var AxisSite = "https://www.axis.com"

// UserAgent is sent to axis.com (set by the programs to include their version). An own
// user agent on purpose: axis.com serves protected files without sign-in to some
// generic clients – this tool always goes through the My Axis sign-in as intended.
var UserAgent = "axis-ip-tool"

// MyAxisCookie is the name of the My Axis session cookie.
const MyAxisCookie = "axis_connect_session_sid"

// MyAxisLoginURL is the sign-in page; after signing in, axis.com continues to the
// firmware directory (a page outside the login paths → the cookie is set).
const MyAxisLoginURL = "https://www.axis.com/my-axis/login?axis_destination=https%3A%2F%2Fwww.axis.com%2Fftp%2Fpub_soft%2F"

// ErrMyAxisLogin: the download needs a (new) sign-in with an Axis account.
var ErrMyAxisLogin = errors.New("sign in with your Axis account (My Axis) to download firmware")

// firmwareSources: cameras etc. (MPQT) first, then audio, access control, I/O (PACS).
var firmwareSources = []string{"MPQT", "PACS"}

// ltsTracks are the AXIS OS long-term support tracks (major.minor).
var ltsTracks = map[[2]int]string{
	{12, 11}: "LTS 2026", {11, 11}: "LTS 2024", {10, 12}: "LTS 2022",
	{9, 80}: "LTS 2020", {8, 40}: "LTS 2018", {6, 50}: "LTS 2016",
}

// ---------------------------------------------------------------- Versions

// FWVersion is a parsed firmware version like 12.11.77.
type FWVersion []int

// ParseFWVersion reads "12.11.77" or "12_11_77" (2–5 numeric parts). Suffixes like
// "-beta" or "_TTA" are not valid.
func ParseFWVersion(s string) (FWVersion, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' })
	if len(parts) < 2 || len(parts) > 5 {
		return nil, false
	}
	v := make(FWVersion, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		v[i] = n
	}
	return v, true
}

func (v FWVersion) String() string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

func (v FWVersion) part(i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// Compare returns -1, 0 or 1.
func (v FWVersion) Compare(o FWVersion) int {
	for i := 0; i < max(len(v), len(o)); i++ {
		if a, b := v.part(i), o.part(i); a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Track is major.minor.
func (v FWVersion) Track() [2]int { return [2]int{v.part(0), v.part(1)} }

// LTS reports whether the version belongs to a long-term support track.
func (v FWVersion) LTS() bool { _, ok := ltsTracks[v.Track()]; return ok }

// TrackName describes the track for display ("LTS 2024 (11.11)", "AXIS OS 12").
func (v FWVersion) TrackName() string {
	t := v.Track()
	if n, ok := ltsTracks[t]; ok {
		return fmt.Sprintf("%s (%d.%d)", n, t[0], t[1])
	}
	return fmt.Sprintf("AXIS OS %d (active)", t[0])
}

// ---------------------------------------------------------------- Listings

// listingEntry is one row of an axis.com directory listing.
type listingEntry struct {
	Name   string // without trailing slash
	Dir    bool
	Href   string
	Size   int64 // approximate (listing rounds, e.g. "55.6M")
	SHA256 string
}

var (
	rowRe  = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
	hrefRe = regexp.MustCompile(`<a href="([^"]*)">([^<]*)</a>`)
	sizeRe = regexp.MustCompile(`<td class="size">([^<]*)</td>`)
	shaRe  = regexp.MustCompile(`<td class="sha256">([0-9a-fA-F]{64})</td>`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
)

func parseListing(page string) []listingEntry {
	var out []listingEntry
	for _, m := range rowRe.FindAllStringSubmatch(page, -1) {
		a := hrefRe.FindStringSubmatch(m[1])
		if a == nil {
			continue
		}
		name := html.UnescapeString(a[2])
		e := listingEntry{Href: html.UnescapeString(a[1]), Dir: strings.HasSuffix(name, "/")}
		e.Name = strings.TrimSuffix(name, "/")
		if !nameRe.MatchString(e.Name) {
			continue // "Parent Directory" etc.
		}
		if s := sizeRe.FindStringSubmatch(m[1]); s != nil {
			e.Size = parseSize(s[1])
		}
		if s := shaRe.FindStringSubmatch(m[1]); s != nil {
			e.SHA256 = strings.ToLower(s[1])
		}
		out = append(out, e)
	}
	return out
}

// parseSize reads "55.6M", "999.8K", "8" (units of 1024).
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	mult := 1.0
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		}
		if mult > 1 {
			s = s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * mult)
}

// FirmwareCatalog reads the axis.com listings and caches them for a while.
type FirmwareCatalog struct {
	hc *http.Client

	mu    sync.Mutex
	cache map[string]cachedPage
}

type cachedPage struct {
	entries []listingEntry
	missing bool
	at      time.Time
}

const catalogTTL = 30 * time.Minute

// NewFirmwareCatalog creates a catalog client. Redirects are not followed: axis.com
// answers missing pages and protected files with redirects.
func NewFirmwareCatalog() *FirmwareCatalog {
	return &FirmwareCatalog{
		hc: &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cache: map[string]cachedPage{},
	}
}

// list fetches a directory listing (path below AxisSite, ending with "/").
// A missing page returns nil entries without error.
func (fc *FirmwareCatalog) list(ctx context.Context, path string) ([]listingEntry, error) {
	fc.mu.Lock()
	if p, ok := fc.cache[path]; ok && time.Since(p.at) < catalogTTL {
		fc.mu.Unlock()
		return p.entries, nil
	}
	fc.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, AxisSite+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := fc.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("axis.com: %w", err)
	}
	defer resp.Body.Close()
	var page cachedPage
	switch {
	case resp.StatusCode == http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("axis.com: %w", err)
		}
		page.entries = parseListing(string(data))
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
		(resp.StatusCode >= 300 && resp.StatusCode < 400):
		page.missing = true // axis.com redirects missing pages to "Page not found"
	default:
		return nil, fmt.Errorf("axis.com: HTTP %d for %s", resp.StatusCode, path)
	}
	page.at = time.Now()
	fc.mu.Lock()
	fc.cache[path] = page
	fc.mu.Unlock()
	return page.entries, nil
}

// normModel: "AXIS P3225-VE Mk II" → "p3225vemkii" (comparable with the folder names).
func normModel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 5 && strings.EqualFold(s[:5], "axis ") {
		s = s[5:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// FirmwareProduct is the product folder on axis.com that belongs to a device model.
type FirmwareProduct struct {
	Source string // MPQT or PACS
	Folder string // e.g. P3225-VE_Mk_II
}

func (p FirmwareProduct) path() string {
	return "/ftp/pub_soft/" + p.Source + "/" + p.Folder + "/"
}

// FindProduct looks up the product folder for the model names of a device (product
// number like "P3225-VE Mk II" first, then the full name). An exact match wins;
// otherwise the longest folder name the model starts with (e.g. "Q2101-TE Thermal Camera"
// → Q2101-TE). ok is false if axis.com has no firmware for the model.
func (fc *FirmwareCatalog) FindProduct(ctx context.Context, models ...string) (FirmwareProduct, bool, error) {
	type folder struct {
		FirmwareProduct
		norm string
	}
	var all []folder
	for _, src := range firmwareSources {
		entries, err := fc.list(ctx, "/ftp/pub_soft/"+src+"/")
		if err != nil {
			return FirmwareProduct{}, false, err
		}
		for _, e := range entries {
			if e.Dir {
				all = append(all, folder{FirmwareProduct{src, e.Name}, normModel(e.Name)})
			}
		}
	}
	for _, m := range models {
		if n := normModel(m); n != "" {
			for _, f := range all { // MPQT comes first
				if f.norm == n {
					return f.FirmwareProduct, true, nil
				}
			}
		}
	}
	var best folder
	for _, m := range models {
		n := normModel(m)
		for _, f := range all {
			if len(f.norm) >= 4 && strings.HasPrefix(n, f.norm) && len(f.norm) > len(best.norm) {
				best = f
			}
		}
	}
	return best.FirmwareProduct, best.norm != "", nil
}

// Versions lists all firmware versions of a product, newest first.
func (fc *FirmwareCatalog) Versions(ctx context.Context, p FirmwareProduct) ([]FWVersion, error) {
	entries, err := fc.list(ctx, p.path())
	if err != nil {
		return nil, err
	}
	var out []FWVersion
	for _, e := range entries {
		if v, ok := ParseFWVersion(e.Name); ok && e.Dir && v.part(0) >= 5 {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) > 0 })
	return out, nil
}

// FirmwareFile is a downloadable firmware image.
type FirmwareFile struct {
	Product  FirmwareProduct
	Version  FWVersion
	Name     string // e.g. P3225-VE_Mk_II_9_80_132.bin
	URL      string
	Size     int64 // approximate (from the listing)
	SHA256   string
	Uploaded string // date from the listing ("" = unknown)
}

// File returns the .bin of a version.
func (fc *FirmwareCatalog) File(ctx context.Context, p FirmwareProduct, v FWVersion) (FirmwareFile, error) {
	dir := p.path() + strings.ReplaceAll(v.String(), ".", "_") + "/"
	entries, err := fc.list(ctx, dir)
	if err != nil {
		return FirmwareFile{}, err
	}
	for _, e := range entries {
		if !e.Dir && strings.HasSuffix(strings.ToLower(e.Name), ".bin") && e.SHA256 != "" && e.Size >= 1<<20 {
			return FirmwareFile{Product: p, Version: v, Name: e.Name, URL: AxisSite + dir + e.Name,
				Size: e.Size, SHA256: e.SHA256}, nil
		}
	}
	return FirmwareFile{}, fmt.Errorf("no firmware file for %s %s on axis.com", p.Folder, v)
}

// FirmwareChoice is a suggested target version for a device.
type FirmwareChoice struct {
	Label   string // "Current track", "Latest LTS", "Latest"
	Version FWVersion
}

// FirmwareChoices suggests target versions for a device with firmware current:
// the newest version of its track (LTS track: same major.minor; otherwise same major),
// the newest LTS and the newest version overall. Duplicates are merged; versions not
// newer than current are left out unless nothing else exists.
func FirmwareChoices(current FWVersion, versions []FWVersion) []FirmwareChoice {
	var out []FirmwareChoice
	add := func(label string, pick func(FWVersion) bool) {
		for _, v := range versions { // newest first
			if pick(v) {
				for i, c := range out {
					if c.Version.Compare(v) == 0 {
						out[i].Label += " · " + label
						return
					}
				}
				out = append(out, FirmwareChoice{label, v})
				return
			}
		}
	}
	if current != nil {
		if current.LTS() {
			add("Current track", func(v FWVersion) bool { return v.Track() == current.Track() })
		} else {
			add("Current track", func(v FWVersion) bool { return v.part(0) == current.part(0) })
		}
	}
	add("Latest LTS", FWVersion.LTS)
	add("Latest", func(FWVersion) bool { return true })
	if current == nil {
		return out
	}
	newer := out[:0:0]
	for _, c := range out {
		if c.Version.Compare(current) > 0 {
			newer = append(newer, c)
		}
	}
	return newer
}

// ---------------------------------------------------------------- Download

// CheckMyAxisSession tests a session cookie with a HEAD request on a protected file.
// Returns ErrMyAxisLogin if axis.com does not accept it.
func (fc *FirmwareCatalog) CheckMyAxisSession(ctx context.Context, session string) error {
	if !validCookieValue(session) {
		return ErrMyAxisLogin
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		AxisSite+"/ftp/pub_soft/MPQT/P3225-VE_Mk_II/latest/P3225-VE_Mk_II.bin", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", MyAxisCookie+"="+session)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := fc.hc.Do(req)
	if err != nil {
		return fmt.Errorf("axis.com: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return ErrMyAxisLogin
}

func validCookieValue(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r > '~' || strings.ContainsRune(`;,"\`, r) {
			return false
		}
	}
	return true
}

// FirmwareCacheDir is where downloaded firmware is kept (~/.cache/axis-ip-tool/firmware).
func FirmwareCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "axis-ip-tool", "firmware"), nil
}

// Download fetches the firmware into the cache (or uses the cached copy) and verifies
// the SHA-256 from axis.com. progress (may be nil) receives downloaded and total bytes.
func (fc *FirmwareCatalog) Download(ctx context.Context, f FirmwareFile, session string,
	progress func(done, total int64)) (string, error) {
	base, err := FirmwareCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, f.SHA256)
	dst := filepath.Join(dir, filepath.Base(f.Name))
	if sum, err := fileSHA256(dst); err == nil && sum == f.SHA256 {
		return dst, nil
	}
	if !validCookieValue(session) {
		return "", ErrMyAxisLogin
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", MyAxisCookie+"="+session)
	req.Header.Set("User-Agent", UserAgent)
	hc := &http.Client{CheckRedirect: fc.hc.CheckRedirect} // no overall timeout: large file
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		(resp.StatusCode >= 300 && resp.StatusCode < 400):
		return "", ErrMyAxisLogin
	default:
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total <= 0 {
		total = f.Size
	}
	limit := f.Size + f.Size/50 + 1<<20 // the listing size is rounded

	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := tmp.Write(buf[:n]); err != nil {
				tmp.Close()
				return "", err
			}
			h.Write(buf[:n])
			done += int64(n)
			if done > limit {
				tmp.Close()
				return "", errors.New("download: file is larger than announced on axis.com")
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return "", fmt.Errorf("download: %w", rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.SHA256 {
		return "", errors.New("download does not match the SHA-256 checksum from axis.com – nothing was changed")
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------- Session storage

// MyAxisSession is a stored My Axis sign-in (shared by GUI and CLI).
type MyAxisSession struct {
	Value   string    `json:"value"`
	Expires time.Time `json:"expires,omitempty"` // cookie expiry, zero = unknown
	SavedAt time.Time `json:"saved_at"`
}

func myAxisSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "axis-ip-tool", "myaxis-session.json"), nil
}

// LoadMyAxisSession reads the stored session; $AXIS_MYAXIS_SESSION takes precedence.
// ok is false if there is none or it has expired.
func LoadMyAxisSession() (MyAxisSession, bool) {
	if v := strings.TrimSpace(os.Getenv("AXIS_MYAXIS_SESSION")); v != "" {
		v = strings.TrimPrefix(v, MyAxisCookie+"=")
		return MyAxisSession{Value: v}, validCookieValue(v)
	}
	p, err := myAxisSessionPath()
	if err != nil {
		return MyAxisSession{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return MyAxisSession{}, false
	}
	var s MyAxisSession
	if json.Unmarshal(data, &s) != nil || !validCookieValue(s.Value) ||
		(!s.Expires.IsZero() && time.Now().After(s.Expires)) {
		return MyAxisSession{}, false
	}
	return s, true
}

// SaveMyAxisSession stores the session readable only by the user (0600).
func SaveMyAxisSession(s MyAxisSession) error {
	p, err := myAxisSessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	s.SavedAt = time.Now()
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ForgetMyAxisSession deletes the stored session (sign out).
func ForgetMyAxisSession() error {
	p, err := myAxisSessionPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
