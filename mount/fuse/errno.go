package fuse

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"syscall"

	"github.com/BlueRival/splicefs/client/core"
)

// maxLoggedErrors bounds the memory used to remember which error texts were logged,
// because the texts may contain paths and are therefore unbounded in number.
const maxLoggedErrors = 256

// errnoOf maps an error from the core to the error number the kernel is told. known is
// false for errors the mapping has no rule for; those become EIO.
//
// ErrUnavailable is EIO and never ENOENT: a network outage must not look like deleted
// files.
func errnoOf(err error) (errno syscall.Errno, known bool) {
	switch {
	case errors.Is(err, core.ErrNotExist):
		return syscall.ENOENT, true
	case errors.Is(err, core.ErrNotDir):
		return syscall.ENOTDIR, true
	case errors.Is(err, core.ErrIsDir):
		return syscall.EISDIR, true
	case errors.Is(err, core.ErrUnavailable):
		return syscall.EIO, true
	case errors.Is(err, context.Canceled):
		return syscall.EINTR, true
	case errors.Is(err, context.DeadlineExceeded):
		return syscall.EIO, true
	default:
		return syscall.EIO, false
	}
}

// errorMapper turns core errors into error numbers and logs each unexpected error text
// once, so that a failing node does not produce a log line per request.
type errorMapper struct {
	log  *slog.Logger
	mu   sync.Mutex
	seen map[string]struct{}
}

func newErrorMapper(log *slog.Logger) *errorMapper {
	return &errorMapper{log: log, seen: make(map[string]struct{})}
}

// errno maps err; see errnoOf.
func (m *errorMapper) errno(err error) syscall.Errno {
	errno, known := errnoOf(err)
	if !known {
		m.logOnce(err)
	}
	return errno
}

func (m *errorMapper) logOnce(err error) {
	text := err.Error()
	if m.firstSighting(text) {
		m.log.Error("unexpected error from core, reported as EIO", "error", text)
	}
}

// firstSighting records text and reports whether it is new and there is room to
// remember it.
func (m *errorMapper) firstSighting(text string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.seen[text]; dup || len(m.seen) >= maxLoggedErrors {
		return false
	}
	m.seen[text] = struct{}{}
	return true
}
