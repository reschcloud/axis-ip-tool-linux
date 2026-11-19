// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrStreamEnded: the device closed the stream.
var ErrStreamEnded = errors.New("stream ended")

// StreamOptions selects the MJPEG stream. Zero values = device defaults.
type StreamOptions struct {
	Width int // requested width; height follows 16:9. Rejected → default resolution
	FPS   int // maximum frame rate
}

// Stream receives the live image as MJPEG (axis-cgi/mjpg/video.cgi) and calls frame for
// every JPEG until ctx is cancelled or the connection breaks. frame runs in the calling
// goroutine and may keep the slice. Stream returns ctx.Err() after cancellation.
func (c *Client) Stream(ctx context.Context, opt StreamOptions, frame func(jpeg []byte)) error {
	q := url.Values{}
	if opt.FPS > 0 {
		q.Set("fps", fmt.Sprint(opt.FPS))
	}
	withRes := url.Values{}
	for k, v := range q {
		withRes[k] = v
	}
	if opt.Width > 0 {
		withRes.Set("resolution", fmt.Sprintf("%dx%d", opt.Width, opt.Width*9/16))
	}
	err := c.stream(ctx, withRes, frame)
	if errors.Is(err, errBadRequest) && opt.Width > 0 {
		err = c.stream(ctx, q, frame) // resolution not supported → device default
	}
	return err
}

var errBadRequest = errors.New("bad request")

func (c *Client) stream(ctx context.Context, q url.Values, frame func([]byte)) error {
	path := "/axis-cgi/mjpg/video.cgi"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	// no overall timeout: the response body never ends
	hc := &http.Client{Transport: c.hc.Transport}
	resp, err := c.send(ctx, hc, http.MethodGet, path, nil, "", true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		return errBadRequest
	}
	if err := statusError(resp.StatusCode, path); err != nil {
		return err
	}
	return readMJPEG(ctx, resp.Header.Get("Content-Type"), resp.Body, frame)
}

// maxFrame limits the size of a single JPEG.
const maxFrame = 16 << 20

// readMJPEG splits a multipart/x-mixed-replace body into JPEG frames. Unlike
// mime/multipart it uses the Content-Length of each part (Axis always sends it), so a
// frame is delivered as soon as it is complete – not only when the next one starts.
func readMJPEG(ctx context.Context, ctype string, body io.Reader, frame func([]byte)) error {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return fmt.Errorf("unexpected stream format %q", ctype)
	}
	// some firmware writes the boundary with leading dashes in the header too
	delim := []byte("--" + strings.TrimPrefix(params["boundary"], "--"))
	isDelim := func(line []byte) bool { return bytes.HasPrefix(bytes.TrimSpace(line), delim) }
	fail := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return ErrStreamEnded
		}
		return err
	}

	br := bufio.NewReaderSize(body, 64<<10)
	atPart := false // boundary line already consumed
	for {
		if !atPart {
			for {
				line, err := br.ReadSlice('\n')
				if err != nil && err != bufio.ErrBufferFull {
					return fail(err)
				}
				if isDelim(line) {
					break
				}
			}
		}
		atPart = false
		size := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return fail(err)
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
				size, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		var data []byte
		if size >= 0 && size <= maxFrame {
			data = make([]byte, size)
			if _, err := io.ReadFull(br, data); err != nil {
				return fail(err)
			}
		} else { // no length: read up to the next boundary
			var buf bytes.Buffer
			for {
				line, err := br.ReadBytes('\n')
				if err != nil {
					return fail(err)
				}
				if isDelim(line) {
					atPart = true
					break
				}
				if buf.Len()+len(line) > maxFrame {
					return errors.New("stream frame too large")
				}
				buf.Write(line)
			}
			data = bytes.TrimSuffix(buf.Bytes(), []byte("\r\n"))
		}
		if isJPEG(data) {
			frame(data)
		}
	}
}
