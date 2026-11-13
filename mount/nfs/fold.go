package nfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/SpliceFS/client/core"
)

// resolutionCacheSize bounds how many requested names are remembered together with the
// stored name they matched. Only names that were asked for in another normalization
// form than the stored one get an entry, so this is far more than a session needs; the
// cache exists because go-nfs asks for the same path on every request, and without it
// each READ of such a file would list its directory.
const resolutionCacheSize = 4096

// normalizingFS decorates a core.FS for programs that ask for an accented name in a
// different Unicode normalization form than the stored one, which macOS programs do.
// When Stat, ReadDir or Open answers core.ErrNotExist for a name that is not pure
// ASCII, each path segment that does not exist as asked is replaced by the one entry
// of its directory whose NFC form equals the NFC form of the segment. Names are
// otherwise passed through byte for byte, and a name that matches several entries is
// treated as missing rather than guessed.
type normalizingFS struct {
	core.FS

	resolved *lru[string, string] // requested path -> stored path
}

func newNormalizingFS(fsys core.FS) *normalizingFS {
	return &normalizingFS{FS: fsys, resolved: newLRU[string, string](resolutionCacheSize)}
}

// Stat describes name, finding it under another normalization form if needed.
func (n *normalizingFS) Stat(ctx context.Context, name string) (core.Attr, error) {
	return withFallback(ctx, n, name, func(p string) (core.Attr, error) {
		return n.FS.Stat(ctx, p)
	})
}

// ReadDir lists the directory name, finding it under another normalization form if
// needed. go-nfs keeps the path a program used for a directory and lists it later, so
// the fallback has to apply here as well.
func (n *normalizingFS) ReadDir(ctx context.Context, name string) ([]core.Attr, error) {
	return withFallback(ctx, n, name, func(p string) ([]core.Attr, error) {
		return n.FS.ReadDir(ctx, p)
	})
}

// Open opens name, finding it under another normalization form if needed.
//
//nolint:ireturn // The signature is fixed by core.FS.
func (n *normalizingFS) Open(ctx context.Context, name string) (core.File, error) {
	return withFallback(ctx, n, name, func(p string) (core.File, error) {
		return n.FS.Open(ctx, p)
	})
}

// withFallback runs operation on name and, only when that says the name does not exist and the
// name is not pure ASCII, retries it on the stored spelling of the name.
//
//nolint:ireturn // T is the result type of operation, which the caller chooses; not an interface here.
func withFallback[T any](
	ctx context.Context, n *normalizingFS, name string, operation func(string) (T, error),
) (T, error) {
	result, err := operation(name)
	if !errors.Is(err, core.ErrNotExist) || isASCII(name) {
		return result, err
	}
	// A remembered spelling is only a hint: it is tried, and dropped if the tree has
	// changed since, so no clock is needed to expire it.
	if stored, ok := n.resolved.get(name); ok {
		if retried, retryErr := operation(stored); !errors.Is(retryErr, core.ErrNotExist) {
			return retried, retryErr
		}
		n.resolved.remove(name)
	}
	stored, rerr := n.resolve(ctx, name)
	switch {
	case errors.Is(rerr, core.ErrNotExist) || stored == name:
		return result, err
	case rerr != nil:
		var zero T
		return zero, rerr
	}
	n.resolved.add(name, stored)
	return operation(stored)
}

// resolve returns the stored spelling of name. It lists a directory only for a segment
// that is not ASCII and does not exist as asked, so no directory off the path is read.
func (n *normalizingFS) resolve(ctx context.Context, name string) (string, error) {
	stored := ""
	for seg := range strings.SplitSeq(name, "/") {
		part := seg
		if !isASCII(seg) {
			fixed, err := n.storedSegment(ctx, stored, seg)
			if err != nil {
				return "", err
			}
			part = fixed
		}
		stored = path.Join(stored, part)
	}
	return stored, nil
}

// storedSegment returns seg if it exists in dir, otherwise the single entry of dir
// whose NFC form equals that of seg, otherwise core.ErrNotExist.
func (n *normalizingFS) storedSegment(ctx context.Context, dir, seg string) (string, error) {
	if _, err := n.FS.Stat(ctx, path.Join(dir, seg)); !errors.Is(err, core.ErrNotExist) {
		if err != nil {
			return "", fmt.Errorf("stat %q: %w", path.Join(dir, seg), err)
		}
		return seg, nil
	}
	entries, err := n.FS.ReadDir(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("list %q: %w", dir, err)
	}
	want := norm.NFC.String(seg)
	match := ""
	found := 0
	for _, e := range entries {
		if norm.NFC.String(e.Name) == want {
			match = e.Name
			found++
		}
	}
	if found != 1 {
		return "", core.ErrNotExist
	}
	return match, nil
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
