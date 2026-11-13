package nfs

import (
	"errors"
	"io"
	"path"
	"slices"
	"testing"

	"github.com/SpliceFS/client/core"
)

const (
	nfcCafe = "caf\u00e9.txt"  // e with acute accent, one code point
	nfdCafe = "cafe\u0301.txt" // e followed by a combining acute accent
)

func readAll(t *testing.T, f core.File) string {
	t.Helper()
	buf := make([]byte, 64)
	n, err := f.ReadAt(t.Context(), buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNormalizingFSFindsOtherForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stored    string
		requested string
	}{
		{"stored NFC, asked NFD", "music/" + nfcCafe, "music/" + nfdCafe},
		{"stored NFD, asked NFC", "music/" + nfdCafe, "music/" + nfcCafe},
		{"directory in another form", "m\u00fasica/song", "mu\u0301sica/song"},
		{"every segment in another form", "m\u00fasica/" + nfcCafe, "mu\u0301sica/" + nfdCafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeFS().addFile(tt.stored, []byte("content"))
			n := newNormalizingFS(fsys)

			attr, err := n.Stat(t.Context(), tt.requested)
			if err != nil || attr.Size != 7 {
				t.Fatalf("Stat = %+v, %v", attr, err)
			}
			f, err := n.Open(t.Context(), tt.requested)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, f); got != "content" {
				t.Errorf("content = %q", got)
			}
			// The directory part of the request is a directory, too.
			dir := path.Dir(tt.requested)
			if entries, err := n.ReadDir(t.Context(), dir); err != nil || len(entries) != 1 {
				t.Errorf("ReadDir(%q) = %v, %v", dir, entries, err)
			}
		})
	}
}

func TestNormalizingFSListsOnlyDirectoriesOnThePath(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().
		addFile("m\u00fasica/"+nfcCafe, []byte("x")).
		addFile("other/file", []byte("y")).
		addFile("m\u00fasica/other", []byte("z"))
	n := newNormalizingFS(fsys)
	if _, err := n.Stat(t.Context(), "mu\u0301sica/"+nfdCafe); err != nil {
		t.Fatal(err)
	}
	// "musica" is listed in the root to find its stored spelling, and the stored
	// directory is listed to find the file. Nothing else is.
	if got, want := fsys.listings(), []string{"", "m\u00fasica"}; !slices.Equal(got, want) {
		t.Errorf("listed %q, want %q", got, want)
	}
}

func TestNormalizingFSRemembersWhatItFound(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile(nfcCafe, []byte("x"))
	n := newNormalizingFS(fsys)
	for range 3 {
		if _, err := n.Stat(t.Context(), nfdCafe); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(fsys.listings()); got != 1 {
		t.Errorf("directory listed %d times, want once", got)
	}
}

func TestNormalizingFSForgetsAChangedName(t *testing.T) {
	t.Parallel()
	const asked = "\u212b.txt" // ANGSTROM SIGN: its NFC form is U+00C5
	fsys := newFakeFS().addFile("\u00c5.txt", []byte("old"))
	n := newNormalizingFS(fsys)
	if _, err := n.Stat(t.Context(), asked); err != nil {
		t.Fatal(err)
	}

	fsys.mu.Lock()
	delete(fsys.nodes, "\u00c5.txt")
	fsys.mu.Unlock()
	fsys.addFile("A\u030a.txt", []byte("new")) // the same name in NFD

	f, err := n.Open(t.Context(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f); got != "new" {
		t.Errorf("content = %q, want the renamed file", got)
	}
}

func TestNormalizingFSNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		build     func() *fakeFS
		requested string
	}{
		{"no entry matches", func() *fakeFS { return newFakeFS().addFile("other.txt", nil) }, nfdCafe},
		{"two entries match", func() *fakeFS {
			return newFakeFS().addFile("\u00c5", nil).addFile("A\u030a", nil)
		}, "\u212b"},
		{"parent is missing", newFakeFS, "m\u00fasica/" + nfdCafe},
		{"only an ASCII part is missing", func() *fakeFS { return newFakeFS().addDir(nfcCafe) }, nfcCafe + "/missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := newNormalizingFS(tt.build())
			if _, err := n.Stat(t.Context(), tt.requested); !errors.Is(err, core.ErrNotExist) {
				t.Errorf("Stat = %v, want core.ErrNotExist", err)
			}
		})
	}
}

func TestNormalizingFSDoesNotListForASCIINames(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("a.txt", nil)
	n := newNormalizingFS(fsys)
	if _, err := n.Stat(t.Context(), "b.txt"); !errors.Is(err, core.ErrNotExist) {
		t.Fatalf("Stat = %v", err)
	}
	if got := fsys.listings(); len(got) != 0 {
		t.Errorf("listed %q for an ASCII name", got)
	}
}

func TestNormalizingFSPassesOtherErrorsThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail func(op, name string) error
	}{
		{"the call itself fails", func(op, _ string) error {
			if op == "stat" {
				return core.ErrUnavailable
			}
			return nil
		}},
		{"the probe of a segment fails", func() func(op, name string) error {
			stats := 0
			return func(op, _ string) error {
				if op != "stat" {
					return nil
				}
				stats++
				if stats == 1 {
					return core.ErrNotExist // the request itself
				}
				return core.ErrUnavailable // the probe of its segment
			}
		}()},
		{"the listing fails", func(op, _ string) error {
			if op == "readdir" {
				return core.ErrUnavailable
			}
			return nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFakeFS().addFile(nfcCafe, nil)
			fsys.fail = tt.fail
			n := newNormalizingFS(fsys)
			if _, err := n.Stat(t.Context(), nfdCafe); !errors.Is(err, core.ErrUnavailable) {
				t.Errorf("Stat = %v, want core.ErrUnavailable", err)
			}
		})
	}
}

func TestNormalizingFSPassesStatFSThrough(t *testing.T) {
	t.Parallel()
	n := newNormalizingFS(newFakeFS())
	if got, err := n.StatFS(t.Context()); err != nil || got.Total != 1000 {
		t.Errorf("StatFS = %+v, %v", got, err)
	}
}
