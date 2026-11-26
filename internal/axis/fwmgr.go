// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Firmware upgrade via the Firmware Management API (firmwaremanagement.cgi, "fwmgr").

// FirmwareStatus is the reply of fwmgr "status".
type FirmwareStatus struct {
	ActiveVersion   string `json:"activeFirmwareVersion"`
	InactiveVersion string `json:"inactiveFirmwareVersion"`
	IsCommitted     *bool  `json:"isCommited"` // sic (VAPIX spelling)
	PendingCommit   any    `json:"pendingCommit"`
	TimeToRollback  any    `json:"timeToRollback"`
	LastUpgradeAt   string `json:"lastUpgradeAt"`
}

// GetFirmwareStatus reads the firmware state (admin).
func (c *Client) GetFirmwareStatus(ctx context.Context) (FirmwareStatus, error) {
	var st FirmwareStatus
	err := c.jsonCall(ctx, "firmwaremanagement.cgi", "status", nil, true, &st)
	return st, err
}

// FactoryDefaultMode for the upgrade: keep everything, reset but keep network settings
// ("soft"), or reset completely ("hard").
const (
	FactoryDefaultNone = "none"
	FactoryDefaultSoft = "soft"
	FactoryDefaultHard = "hard"
)

// UpgradeFirmware uploads a firmware file and starts the upgrade; the device then
// installs it and restarts. Returns the version the device reports for the new image.
// progress (may be nil) receives the uploaded bytes.
func (c *Client) UpgradeFirmware(ctx context.Context, path, factoryDefault string,
	progress func(done, total int64)) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() < 1<<20 || fi.Size() > 2<<30 {
		return "", fmt.Errorf("%s does not look like a firmware file (size)", filepath.Base(path))
	}
	if factoryDefault == "" {
		factoryDefault = FactoryDefaultNone
	}
	// cheap authenticated call first: checks admin rights and API support and primes
	// the digest challenge, so the large upload is normally sent only once
	if _, err := c.GetFirmwareStatus(ctx); err != nil {
		if errors.Is(err, ErrNotSupported) {
			return "", fmt.Errorf("firmware upgrade API (firmwaremanagement.cgi): %w", err)
		}
		return "", err
	}

	params, _ := json.Marshal(map[string]any{
		"apiVersion": "1.0", "context": "ip-tool-for-axis", "method": "upgrade",
		"params": map[string]any{"factoryDefaultMode": factoryDefault},
	})
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="json"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	pw.Write(params)
	h = textproto.MIMEHeader{}
	name := strings.Map(func(r rune) rune {
		if r < ' ' || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filepath.Base(path))
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	h.Set("Content-Type", "application/octet-stream")
	mw.CreatePart(h)
	headBytes := append([]byte(nil), head.Bytes()...)
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	total := int64(len(headBytes)) + fi.Size() + int64(len(tail))

	var file *os.File
	defer func() {
		if file != nil {
			file.Close()
		}
	}()
	body := func() (io.Reader, int64, error) {
		if file != nil {
			file.Close()
		}
		var err error
		if file, err = os.Open(path); err != nil {
			return nil, 0, err
		}
		r := io.MultiReader(bytes.NewReader(headBytes), file, strings.NewReader(tail))
		return &progressReader{r: r, total: total, fn: progress}, total, nil
	}
	hc := &http.Client{Transport: c.hc.Transport} // no 8 s timeout: the context limits
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	resp, err := c.sendBody(ctx, hc, http.MethodPost, "/axis-cgi/firmwaremanagement.cgi", body,
		mw.FormDataContentType(), true)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := statusError(resp.StatusCode, "firmwaremanagement.cgi"); err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			FirmwareVersion string `json:"firmwareVersion"`
		} `json:"data"`
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("upgrade: invalid response: %w", err)
	}
	if out.Error != nil {
		out.Error.Method = "upgrade"
		return "", out.Error
	}
	return out.Data.FirmwareVersion, nil
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	last  time.Time
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && (time.Since(p.last) > 200*time.Millisecond || err == io.EOF) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}

// UpgradeStage reports the progress of WaitForUpgrade.
type UpgradeStage int

const (
	StageInstalling UpgradeStage = iota // device still answers with the old firmware
	StageRestarting                     // device is not reachable
	StageDone
)

// WaitForUpgrade waits until the device answers with firmware want after the upload
// (installing → restart → back online). Fails if it comes back with another version
// (rollback) or the timeout expires.
func (c *Client) WaitForUpgrade(ctx context.Context, old, want string, stage func(UpgradeStage)) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	wentOffline := false
	last := UpgradeStage(-1)
	report := func(s UpgradeStage) {
		if s != last && stage != nil {
			last = s
			stage(s)
		}
	}
	report(StageInstalling)
	for {
		select {
		case <-ctx.Done():
			if wentOffline {
				return errors.New("the device did not come back within 20 minutes")
			}
			return errors.New("the device did not restart within 20 minutes")
		case <-time.After(5 * time.Second):
		}
		qctx, qcancel := context.WithTimeout(ctx, 6*time.Second)
		info, err := c.BasicInfo(qctx)
		qcancel()
		switch {
		case err != nil:
			wentOffline = true
			report(StageRestarting)
			// a new firmware may have new certificates/ports: decide the protocol again
			c.mu.Lock()
			c.base = ""
			c.mu.Unlock()
		case want != "" && info.Firmware == want, want == "" && wentOffline && info.Firmware != old:
			report(StageDone)
			return nil
		case wentOffline:
			if info.Firmware == old {
				return fmt.Errorf("the device restarted with the old firmware %s (rolled back?)", old)
			}
			return fmt.Errorf("the device restarted with firmware %s instead of %s", info.Firmware, want)
		default:
			report(StageInstalling)
		}
	}
}

// ProductNumber reads the short product number (e.g. "P3225-VE Mk II"), which matches
// the firmware folders on axis.com better than the full name.
func (c *Client) ProductNumber(ctx context.Context) (string, error) {
	var out struct {
		PropertyList map[string]string `json:"propertyList"`
	}
	err := c.jsonCall(ctx, "basicdeviceinfo.cgi", "getAllUnrestrictedProperties", nil, false, &out)
	if errors.Is(err, ErrAuth) && c.user != "" {
		err = c.jsonCall(ctx, "basicdeviceinfo.cgi", "getAllProperties", nil, true, &out)
	}
	if err == nil {
		return out.PropertyList["ProdNbr"], nil
	}
	if c.user == "" {
		return "", err
	}
	p, perr := c.params(ctx, "Brand.ProdNbr")
	if perr != nil {
		return "", err
	}
	return p["Brand.ProdNbr"], nil
}
