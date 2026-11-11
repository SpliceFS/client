package fuse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	"github.com/BlueRival/splicefs/client/core"
)

func TestErrnoOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		want      syscall.Errno
		wantKnown bool
	}{
		{"not exist", core.ErrNotExist, syscall.ENOENT, true},
		{"not a directory", core.ErrNotDir, syscall.ENOTDIR, true},
		{"is a directory", core.ErrIsDir, syscall.EISDIR, true},
		{"unavailable is I/O error, not ENOENT", core.ErrUnavailable, syscall.EIO, true},
		{"cancelled", context.Canceled, syscall.EINTR, true},
		{"deadline", context.DeadlineExceeded, syscall.EIO, true},
		{"wrapped not exist", fmt.Errorf("stat a: %w", core.ErrNotExist), syscall.ENOENT, true},
		{"wrapped unavailable", fmt.Errorf("stat a: %w", core.ErrUnavailable), syscall.EIO, true},
		{"wrapped cancel", fmt.Errorf("read: %w", context.Canceled), syscall.EINTR, true},
		{"joined", errors.Join(errors.New("x"), core.ErrIsDir), syscall.EISDIR, true},
		{"anything else", errors.New("boom"), syscall.EIO, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, known := errnoOf(tt.err)
			if got != tt.want || known != tt.wantKnown {
				t.Fatalf("errnoOf(%v) = %v, %v; want %v, %v", tt.err, got, known, tt.want, tt.wantKnown)
			}
		})
	}
}

func logLines(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), "\n")
}

func TestErrorMapperLogsEachTextOnce(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	m := newErrorMapper(slog.New(slog.NewTextHandler(&buf, nil)))

	for range 3 {
		if got := m.errno(errors.New("boom")); got != syscall.EIO {
			t.Fatalf("errno = %v, want EIO", got)
		}
	}
	if got := logLines(&buf); got != 1 {
		t.Fatalf("logged %d lines for one repeated text, want 1:\n%s", got, buf.String())
	}
	_ = m.errno(errors.New("different"))
	if got := logLines(&buf); got != 2 {
		t.Fatalf("logged %d lines for two texts, want 2", got)
	}
	if !strings.Contains(buf.String(), "boom") || !strings.Contains(buf.String(), "different") {
		t.Fatalf("log does not contain the error texts:\n%s", buf.String())
	}
}

func TestErrorMapperIsSilentForKnownErrors(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	m := newErrorMapper(slog.New(slog.NewTextHandler(&buf, nil)))
	for _, err := range []error{core.ErrNotExist, core.ErrNotDir, core.ErrIsDir, core.ErrUnavailable, context.Canceled, context.DeadlineExceeded} {
		_ = m.errno(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("known errors were logged:\n%s", buf.String())
	}
}

func TestErrorMapperRemembersBoundedNumberOfTexts(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	m := newErrorMapper(slog.New(slog.NewTextHandler(&buf, nil)))
	for i := range maxLoggedErrors + 10 {
		_ = m.errno(fmt.Errorf("failure %d", i))
	}
	if got := logLines(&buf); got != maxLoggedErrors {
		t.Fatalf("logged %d lines, want the cap of %d", got, maxLoggedErrors)
	}
}
