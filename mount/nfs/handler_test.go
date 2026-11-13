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

// pathOf returns the path that handle stands for.
func pathOf(t *testing.T, nfsHandler *handler, handle []byte) []string {
	t.Helper()
	_, got, err := nfsHandler.FromHandle(handle)
	if err != nil {
		t.Fatalf("FromHandle: %v", err)
	}
	return got
}

func TestHandleRoundTrip(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 10)
	for _, path := range [][]string{{}, {"a"}, {"a", "b", "c.txt"}} {
		handle := nfsHandler.ToHandle(nil, path)
		fsys, got, err := nfsHandler.FromHandle(handle)
		if err != nil {
			t.Fatalf("FromHandle(%q): %v", path, err)
		}
		if fsys != nfsHandler.fs || !slices.Equal(got, path) || got == nil {
			t.Errorf("FromHandle(%q) = %v, %v", path, fsys, got)
		}
		if again := nfsHandler.ToHandle(nil, path); !slices.Equal(again, handle) {
			t.Errorf("handle for %q changed between calls", path)
		}
	}
	if slices.Equal(nfsHandler.ToHandle(nil, []string{"a"}), nfsHandler.ToHandle(nil, []string{"b"})) {
		t.Error("different paths share a handle")
	}
}

func TestFromHandleReturnsACopy(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 10)
	handle := nfsHandler.ToHandle(nil, []string{"a", "b"})
	first := pathOf(t, nfsHandler, handle)
	first[0] = "changed" // go-nfs appends to and modifies what it is given
	second := pathOf(t, nfsHandler, handle)
	if second[0] != "a" {
		t.Errorf("path = %q; the cache was modified through a returned slice", second)
	}
}

func TestFromHandleRejectsUnknownAndMalformedHandles(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 10)
	unknown := nfsHandler.ToHandle(nil, []string{"x"})
	if err := nfsHandler.InvalidateHandle(nil, unknown); err != nil {
		t.Fatal(err)
	}
	for name, handle := range map[string][]byte{
		"unknown":   unknown,
		"empty":     {},
		"too short": {1, 2, 3},
		"too long":  append(slices.Clone(unknown), 0),
	} {
		if _, _, err := nfsHandler.FromHandle(handle); err == nil {
			t.Errorf("%s: no error", name)
		} else {
			wantStale(t, err)
		}
	}
}

func TestHandlesAreEvictedLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 2, 2)
	handleA := nfsHandler.ToHandle(nil, []string{"a"})
	handleB := nfsHandler.ToHandle(nil, []string{"b"})
	if _, _, err := nfsHandler.FromHandle(handleA); err != nil { // a is now more recent than b
		t.Fatal(err)
	}
	nfsHandler.ToHandle(nil, []string{"c"}) // evicts b
	if _, _, err := nfsHandler.FromHandle(handleB); err == nil {
		t.Error("b should have been evicted")
	}
	if _, _, err := nfsHandler.FromHandle(handleA); err != nil {
		t.Errorf("a was evicted: %v", err)
	}
	// A handle is a function of its path, so handing the path out again revives it.
	if again := nfsHandler.ToHandle(nil, []string{"b"}); !slices.Equal(again, handleB) {
		t.Error("the handle for b changed after eviction")
	}
	if _, _, err := nfsHandler.FromHandle(handleB); err != nil {
		t.Errorf("b did not come back: %v", err)
	}
}

func TestInvalidateHandle(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 10)
	handle := nfsHandler.ToHandle(nil, []string{"a"})
	if err := nfsHandler.InvalidateHandle(nil, []byte{1, 2}); err != nil { // malformed: ignored
		t.Error(err)
	}
	if err := nfsHandler.InvalidateHandle(nil, handle); err != nil {
		t.Error(err)
	}
	if _, _, err := nfsHandler.FromHandle(handle); err == nil {
		t.Error("the handle survived InvalidateHandle")
	}
}

func TestHandlerMountChangeAndLimit(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 77, 10)
	status, fsys, flavors := nfsHandler.Mount(t.Context(), nil, gonfs.MountRequest{Dirpath: []byte("/anything")})
	wantFlavors := []gonfs.AuthFlavor{gonfs.AuthFlavorNull}
	if status != gonfs.MountStatusOk || fsys != nfsHandler.fs || !slices.Equal(flavors, wantFlavors) {
		t.Errorf("Mount = %v, %v, %v", status, fsys, flavors)
	}
	if nfsHandler.Change(nfsHandler.fs) != nil {
		t.Error("Change is not nil: go-nfs would allow attribute changes")
	}
	if nfsHandler.HandleLimit() != 77 {
		t.Errorf("HandleLimit() = %d", nfsHandler.HandleLimit())
	}
}

func TestFSStatReportsTheCapacity(t *testing.T) {
	t.Parallel()
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 10)
	var stat gonfs.FSStat
	if err := nfsHandler.FSStat(t.Context(), nfsHandler.fs, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.TotalSize != 1000 || stat.FreeSize != 400 || stat.AvailableSize != 400 {
		t.Errorf("FSStat = %+v", stat)
	}
}

func TestFSStatOfAnUnreachableViewReportsTheLastCapacity(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS()
	nfsHandler := newTestHandler(t, fsys, 10, 10)
	// Before any answer the capacity is zero, and it is still not an error: the
	// kernel asks while it mounts, and a mount must succeed with no node reachable.
	fsys.fail = func(string, string) error { return core.ErrUnavailable }
	var stat gonfs.FSStat
	if err := nfsHandler.FSStat(t.Context(), nfsHandler.fs, &stat); err != nil || stat.TotalSize != 0 {
		t.Fatalf("FSStat = %+v, %v, want zero capacity and no error", stat, err)
	}
	fsys.fail = nil
	if err := nfsHandler.FSStat(t.Context(), nfsHandler.fs, &stat); err != nil || stat.TotalSize != 1000 {
		t.Fatalf("FSStat = %+v, %v, want the real capacity", stat, err)
	}
	fsys.fail = func(string, string) error { return core.ErrUnavailable }
	stat = gonfs.FSStat{}
	if err := nfsHandler.FSStat(t.Context(), nfsHandler.fs, &stat); err != nil || stat.TotalSize != 1000 {
		t.Errorf("FSStat = %+v, %v, want the last capacity", stat, err)
	}
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
	nfsHandler := newTestHandler(t, newFakeFS(), 10, 2)
	listing := infos("a", "b")
	verifier := nfsHandler.VerifierFor("dir", listing)
	if verifier == 0 {
		t.Fatal("the verifier is 0, which NFS reserves")
	}
	if got := nfsHandler.DataForVerifier("dir", verifier); !slices.Equal(got, listing) {
		t.Errorf("DataForVerifier = %v", got)
	}
	if again := nfsHandler.VerifierFor("dir", infos("a", "b")); again != verifier {
		t.Error("the verifier of an unchanged listing changed")
	}
	for name, other := range map[string]uint64{
		"another path":    nfsHandler.VerifierFor("other", infos("a", "b")),
		"another entries": nfsHandler.VerifierFor("dir", infos("a", "c")),
	} {
		if other == verifier {
			t.Errorf("%s: same verifier", name)
		}
	}
	if got := nfsHandler.DataForVerifier("wrong path", verifier); got != nil {
		t.Errorf("DataForVerifier for another path = %v", got)
	}
	if got := nfsHandler.DataForVerifier("dir", 12345); got != nil {
		t.Errorf("DataForVerifier for an unknown verifier = %v", got)
	}
}

func TestReserveZero(t *testing.T) {
	t.Parallel()
	if reserveZero(0) == 0 || reserveZero(7) != 7 {
		t.Error("reserveZero must replace 0 and keep every other value")
	}
}
