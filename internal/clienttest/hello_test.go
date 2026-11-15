package clienttest_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/internal/clienttest"
)

func TestHelloStat(t *testing.T) {
	t.Parallel()
	ctx, fsys := context.Background(), clienttest.HelloFS{}
	root, err := fsys.Stat(ctx, "")
	if err != nil || !root.Dir {
		t.Errorf("Stat of the root = %+v, %v", root, err)
	}
	file, err := fsys.Stat(ctx, clienttest.HelloName)
	if err != nil || file.Size != int64(len(clienttest.HelloText)) {
		t.Errorf("Stat of the file = %+v, %v", file, err)
	}
	if _, err = fsys.Stat(ctx, "other"); !errors.Is(err, core.ErrNotExist) {
		t.Errorf("Stat of another name = %v", err)
	}
}

func TestHelloReadDir(t *testing.T) {
	t.Parallel()
	ctx, fsys := context.Background(), clienttest.HelloFS{}
	entries, err := fsys.ReadDir(ctx, "")
	if err != nil || len(entries) != 1 || entries[0].Name != clienttest.HelloName {
		t.Errorf("ReadDir of the root = %+v, %v", entries, err)
	}
	if _, err = fsys.ReadDir(ctx, "other"); !errors.Is(err, core.ErrNotExist) {
		t.Errorf("ReadDir of another folder = %v", err)
	}
}

func TestHelloOpenAndSpace(t *testing.T) {
	t.Parallel()
	ctx, fsys := context.Background(), clienttest.HelloFS{}
	if _, err := fsys.Open(ctx, "other"); !errors.Is(err, core.ErrNotExist) {
		t.Errorf("Open of another name = %v", err)
	}
	space, err := fsys.StatFS(ctx)
	if err != nil || space.Total == 0 {
		t.Errorf("StatFS = %+v, %v", space, err)
	}
}

func TestHelloFileReads(t *testing.T) {
	t.Parallel()
	ctx, fsys := context.Background(), clienttest.HelloFS{}
	file, err := fsys.Open(ctx, clienttest.HelloName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // the double's Close cannot fail
	if file.Attr().Name != clienttest.HelloName {
		t.Errorf("Attr = %+v", file.Attr())
	}
	tests := []struct {
		name    string
		off     int64
		size    int
		want    string
		wantEOF bool
	}{
		{"all", 0, 100, clienttest.HelloText, true},
		{"exactly to the end", 0, len(clienttest.HelloText), clienttest.HelloText, true},
		{"the start", 0, 5, clienttest.HelloText[:5], false},
		{"the middle", 6, 4, clienttest.HelloText[6:10], false},
		{"past the end", int64(len(clienttest.HelloText)), 4, "", true},
	}
	for _, tt := range tests {
		buf := make([]byte, tt.size)
		n, err := file.ReadAt(ctx, buf, tt.off)
		if string(buf[:n]) != tt.want || errors.Is(err, io.EOF) != tt.wantEOF {
			t.Errorf("%s: ReadAt = %q, %v; want %q, EOF %v", tt.name, buf[:n], err, tt.want, tt.wantEOF)
		}
	}
}
