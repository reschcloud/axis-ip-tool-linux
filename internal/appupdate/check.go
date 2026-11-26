// SPDX-License-Identifier: AGPL-3.0-or-later

// Package appupdate checks for releases of the app itself, not camera firmware.
package appupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"
)

const repository = "reschcloud/ip-tool-for-axis-linux"

// Check returns the tag of a newer stable release, or an empty tag when none is available.
func Check(ctx context.Context, installed string) (string, error) {
	return check(ctx, &http.Client{Timeout: 5 * time.Second},
		"https://api.github.com/repos/"+repository+"/releases/latest", installed)
}

// ReleaseURL returns the GitHub page for a tag returned by Check.
func ReleaseURL(tag string) string {
	return "https://github.com/" + repository + "/releases/tag/" + tag
}

func check(ctx context.Context, client *http.Client, endpoint, installed string) (string, error) {
	current, ok := parseVersion(installed, true)
	if !ok { // Unversioned development builds cannot be compared.
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ip-tool-for-axis")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // No published stable release.
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: HTTP %d", resp.StatusCode)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", err
	}
	latest, ok := parseVersion(release.Tag, false)
	if !ok || release.Draft || release.Prerelease || slices.Compare(latest[:], current[:]) <= 0 {
		return "", nil
	}
	return release.Tag, nil
}

var versionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(\.[0-9]+\.g[0-9a-f]+)?$`)

func parseVersion(s string, allowSnapshot bool) ([3]int, bool) {
	var v [3]int
	m := versionPattern.FindStringSubmatch(s)
	if m == nil || (!allowSnapshot && m[4] != "") {
		return v, false
	}
	// packaging/scripts/version.sh appends .N.gHASH to builds after a tag.
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
