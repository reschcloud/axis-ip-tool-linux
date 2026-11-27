// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

func TestIgnoreDisconnect(t *testing.T) {
	if err := ignoreDisconnect(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	if err := ignoreDisconnect(io.EOF); err != nil {
		t.Fatalf("EOF: %v", err)
	}
	read := &net.OpError{Op: "read", Err: errors.New("reset")}
	if err := ignoreDisconnect(read); err != nil {
		t.Fatalf("read: %v", err)
	}
	dial := &net.OpError{Op: "dial", Err: errors.New("timeout")}
	if err := ignoreDisconnect(dial); err == nil {
		t.Fatal("dial timeout treated as success")
	}
	if err := ignoreDisconnect(context.DeadlineExceeded); err == nil {
		t.Fatal("context timeout treated as success")
	}
}
