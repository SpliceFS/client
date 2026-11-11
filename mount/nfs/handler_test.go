package nfs

import (
	"errors"
	"io/fs"
	"slices"
	"testing"

	gonfs "github.com/willscott/go-nfs"

	"github.com/SpliceFS/client/core"
)

func newTestHandler(t *testing.T, fsys core.FS, handleLimit, verifierLimit int) *handler {
	t.Helper()
	return newHandler(newTestBilly(t, fsys), handleLimit, verifierLimit)
}

func wantStale(t *testing.T, err error) {
	t.Helper()
	var status *gonfs.NFSStatusError
	if !errors.As(err, &status) || status.NFSStatus != gonfs.NFSStatusStale {
		t.Errorf("error = %v, want NFS3ERR_STALE", err)
	}
}

func TestHandleRoundTrip(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 10, 10)
	for _, p := range [][]string{{}, {"a"}, {"a", "b", "c.txt"}} {
		fh := h.ToHandle(nil, p)
		fsys, got, err := h.FromHandle(fh)
		if err != nil {
			t.Fatalf("FromHandle(%q): %v", p, err)
		}
		if fsys != h.fs || !slices.Equal(got, p) || got == nil {
			t.Errorf("FromHandle(%q) = %v, %v", p, fsys, got)
		}
		if again := h.ToHandle(nil, p); !slices.Equal(again, fh) {
			t.Errorf("handle for %q changed between calls", p)
		}
	}
	if slices.Equal(h.ToHandle(nil, []string{"a"}), h.ToHandle(nil, []string{"b"})) {
		t.Error("different paths share a handle")
	}
}

func TestFromHandleReturnsACopy(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 10, 10)
	fh := h.ToHandle(nil, []string{"a", "b"})
	_, first, _ := h.FromHandle(fh)
	first[0] = "changed" // go-nfs appends to and modifies what it is given
	_, second, _ := h.FromHandle(fh)
	if second[0] != "a" {
		t.Errorf("path = %q; the cache was modified through a returned slice", second)
	}
}

func TestFromHandleRejectsUnknownAndMalformedHandles(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 10, 10)
	unknown := h.ToHandle(nil, []string{"x"})
	_ = h.InvalidateHandle(nil, unknown)
	for name, fh := range map[string][]byte{
		"unknown":   unknown,
		"empty":     {},
		"too short": {1, 2, 3},
		"too long":  append(slices.Clone(unknown), 0),
	} {
		if _, _, err := h.FromHandle(fh); err == nil {
			t.Errorf("%s: no error", name)
		} else {
			wantStale(t, err)
		}
	}
}

func TestHandlesAreEvictedLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 2, 2)
	a := h.ToHandle(nil, []string{"a"})
	b := h.ToHandle(nil, []string{"b"})
	if _, _, err := h.FromHandle(a); err != nil { // a is now more recent than b
		t.Fatal(err)
	}
	h.ToHandle(nil, []string{"c"}) // evicts b
	if _, _, err := h.FromHandle(b); err == nil {
		t.Error("b should have been evicted")
	}
	if _, _, err := h.FromHandle(a); err != nil {
		t.Errorf("a was evicted: %v", err)
	}
	// A handle is a function of its path, so handing the path out again revives it.
	if again := h.ToHandle(nil, []string{"b"}); !slices.Equal(again, b) {
		t.Error("the handle for b changed after eviction")
	}
	if _, _, err := h.FromHandle(b); err != nil {
		t.Errorf("b did not come back: %v", err)
	}
}

func TestInvalidateHandle(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 10, 10)
	fh := h.ToHandle(nil, []string{"a"})
	if err := h.InvalidateHandle(nil, []byte{1, 2}); err != nil { // malformed: ignored
		t.Error(err)
	}
	if err := h.InvalidateHandle(nil, fh); err != nil {
		t.Error(err)
	}
	if _, _, err := h.FromHandle(fh); err == nil {
		t.Error("the handle survived InvalidateHandle")
	}
}

func TestHandlerMountChangeAndLimit(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 77, 10)
	status, fsys, flavors := h.Mount(t.Context(), nil, gonfs.MountRequest{Dirpath: []byte("/anything")})
	if status != gonfs.MountStatusOk || fsys != h.fs || !slices.Equal(flavors, []gonfs.AuthFlavor{gonfs.AuthFlavorNull}) {
		t.Errorf("Mount = %v, %v, %v", status, fsys, flavors)
	}
	if h.Change(h.fs) != nil {
		t.Error("Change is not nil: go-nfs would allow attribute changes")
	}
	if h.HandleLimit() != 77 {
		t.Errorf("HandleLimit() = %d", h.HandleLimit())
	}
}

func TestFSStat(t *testing.T) {
	t.Parallel()
	t.Run("reports the capacity", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, newFakeFS(), 10, 10)
		var stat gonfs.FSStat
		if err := h.FSStat(t.Context(), h.fs, &stat); err != nil {
			t.Fatal(err)
		}
		if stat.TotalSize != 1000 || stat.FreeSize != 400 || stat.AvailableSize != 400 {
			t.Errorf("FSStat = %+v", stat)
		}
	})
	t.Run("an unreachable view is an I/O error", func(t *testing.T) {
		t.Parallel()
		fsys := newFakeFS()
		fsys.fail = func(string, string) error { return core.ErrUnavailable }
		h := newTestHandler(t, fsys, 10, 10)
		var status *gonfs.NFSStatusError
		err := h.FSStat(t.Context(), h.fs, &gonfs.FSStat{})
		if !errors.As(err, &status) || status.NFSStatus != gonfs.NFSStatusIO {
			t.Errorf("FSStat = %v, want NFS3ERR_IO", err)
		}
	})
}

type namedInfo struct {
	fs.FileInfo
	name string
}

func (n namedInfo) Name() string { return n.name }

func infos(names ...string) []fs.FileInfo {
	out := make([]fs.FileInfo, len(names))
	for i, n := range names {
		out[i] = namedInfo{name: n}
	}
	return out
}

func TestVerifiers(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeFS(), 10, 2)
	listing := infos("a", "b")
	v := h.VerifierFor("dir", listing)
	if v == 0 {
		t.Fatal("the verifier is 0, which NFS reserves")
	}
	if got := h.DataForVerifier("dir", v); !slices.Equal(got, listing) {
		t.Errorf("DataForVerifier = %v", got)
	}
	if again := h.VerifierFor("dir", infos("a", "b")); again != v {
		t.Error("the verifier of an unchanged listing changed")
	}
	for name, other := range map[string]uint64{
		"another path":    h.VerifierFor("other", infos("a", "b")),
		"another entries": h.VerifierFor("dir", infos("a", "c")),
	} {
		if other == v {
			t.Errorf("%s: same verifier", name)
		}
	}
	if got := h.DataForVerifier("wrong path", v); got != nil {
		t.Errorf("DataForVerifier for another path = %v", got)
	}
	if got := h.DataForVerifier("dir", 12345); got != nil {
		t.Errorf("DataForVerifier for an unknown verifier = %v", got)
	}
}

func TestReserveZero(t *testing.T) {
	t.Parallel()
	if reserveZero(0) == 0 || reserveZero(7) != 7 {
		t.Error("reserveZero must replace 0 and keep every other value")
	}
}
