package remote

import (
	"errors"
	"io"
	"strings"
	"testing"
)

var errCloseFailed = errors.New("close failed")

// failingCloser reads from a string and fails to close.
type failingCloser struct {
	io.Reader
}

func (failingCloser) Close() error { return errCloseFailed }

func TestExactReaderCloseReportsTheBodysError(t *testing.T) {
	t.Parallel()
	reader := &exactReader{body: failingCloser{strings.NewReader("abc")}, remaining: 3}
	if err := reader.Close(); !errors.Is(err, errCloseFailed) {
		t.Fatalf("Close error = %v, want %v", err, errCloseFailed)
	}
}
