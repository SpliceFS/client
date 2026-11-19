package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tempFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close: %v", err)
		}
	})
	return file
}

func TestOrderedSyncUsesBarrier(t *testing.T) {
	t.Parallel()
	if err := orderedSync(tempFile(t)); err != nil {
		t.Fatalf("orderedSync: %v", err)
	}
}

func TestBarrierSyncCallsBarrierOnFile(t *testing.T) {
	t.Parallel()
	file := tempFile(t)
	var got []uintptr
	barrier := func(fd uintptr) error {
		got = append(got, fd)
		return nil
	}
	if err := barrierSync(file, barrier); err != nil {
		t.Fatalf("barrierSync: %v", err)
	}
	if len(got) != 1 || got[0] != file.Fd() {
		t.Errorf("barrier called with %v, want [%d]", got, file.Fd())
	}
}

func TestBarrierSyncFallsBackToFullSync(t *testing.T) {
	t.Parallel()
	unsupported := func(uintptr) error { return errInjected }
	cases := []struct {
		name    string
		closed  bool
		wantErr bool
	}{
		{name: "full sync succeeds", closed: false, wantErr: false},
		// A closed file makes the full sync fail, which shows the fallback ran.
		{name: "full sync fails", closed: true, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := tempFile(t)
			if test.closed {
				if err := file.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
			}
			err := barrierSync(file, unsupported)
			if (err != nil) != test.wantErr {
				t.Fatalf("barrierSync error %v, want error %v", err, test.wantErr)
			}
			if err != nil && !errors.Is(err, os.ErrClosed) {
				t.Errorf("barrierSync error %v, want the full sync's error", err)
			}
		})
	}
}

func TestFcntlBarrierReportsFailure(t *testing.T) {
	t.Parallel()
	// No process has this descriptor, so the call fails the way an unsupported one does.
	const invalid = ^uintptr(0)
	if err := fcntlBarrier(invalid); err == nil {
		t.Error("fcntlBarrier on an invalid descriptor succeeded")
	}
}
