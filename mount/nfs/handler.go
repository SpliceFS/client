package nfs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"net"
	"strings"

	"github.com/go-git/go-billy/v5"
	gonfs "github.com/willscott/go-nfs"
)

const (
	// handleCacheSize is how many file handles are remembered. A handle is dropped
	// only after this many other paths were used since its last use, and a program
	// that still holds one has it refreshed at least every 30 seconds, because the
	// kernel revalidates a node when its cached attributes expire. Only nodes that
	// have been idle far longer can go stale, and the kernel then looks the path up
	// again. The size is half of the kernel's default vnode limit on a Mac (263168),
	// which is more nodes than a session touches. An entry is a path of about 100
	// bytes plus 100 bytes of bookkeeping, so a full cache is about 25 MB.
	handleCacheSize = 1 << 17

	// verifierCacheSize is how many directory listings are kept so that a client that
	// reads a large directory in several requests gets one consistent listing without
	// the directory being listed again for every request. Each entry holds a whole
	// listing, which can be hundreds of thousands of entries, and only the listings
	// being paged through right now matter, so the number is kept small.
	verifierCacheSize = 16
)

// handleKey is the opaque NFS file handle: the SHA-256 of the path. Deriving it from the
// path makes a handle the same every time the path is handed out, so a handle that was
// dropped from the cache and a handle that was handed out again are the same bytes and
// the client's copy works again. The client never sees a path or an inode.
type handleKey [sha256.Size]byte

// listing is a directory listing remembered for paged READDIR requests.
type listing struct {
	path    string
	entries []fs.FileInfo
}

// handler is the gonfs.Handler of one mount. It also implements gonfs.CachingHandler,
// which go-nfs detects by type assertion, to page through directory listings.
//
// go-nfs ships a caching handler in its helpers package. It is not used because that
// package needs two further modules (a UUID generator and an LRU cache) that this
// module does not depend on, and because handles derived from the path need neither a
// reverse index nor random identifiers.
type handler struct {
	fs          *billyFS
	handleLimit int
	handles     *lru[handleKey, string]
	verifiers   *lru[uint64, listing]
}

var (
	_ gonfs.Handler        = (*handler)(nil)
	_ gonfs.CachingHandler = (*handler)(nil)
)

func newHandler(fsys *billyFS, handleLimit, verifierLimit int) *handler {
	return &handler{
		fs:          fsys,
		handleLimit: handleLimit,
		handles:     newLRU[handleKey, string](handleLimit),
		verifiers:   newLRU[uint64, listing](verifierLimit),
	}
}

// Mount accepts every mount request with null authentication. The directory path of the
// request is ignored: the client puts the name of the filesystem there, and there is
// only one filesystem to give. Who may connect is decided when the connection is
// accepted.
//
//nolint:ireturn // The signature is fixed by gonfs.Handler.
func (h *handler) Mount(
	context.Context, net.Conn, gonfs.MountRequest,
) (gonfs.MountStatus, billy.Filesystem, []gonfs.AuthFlavor) {
	return gonfs.MountStatusOk, h.fs, []gonfs.AuthFlavor{gonfs.AuthFlavorNull}
}

// Change returns nil, which tells go-nfs that attributes cannot be changed.
//
//nolint:ireturn // The signature is fixed by gonfs.Handler.
func (*handler) Change(billy.Filesystem) billy.Change {
	return nil
}

// FSStat reports the capacity of the view.
func (h *handler) FSStat(ctx context.Context, _ billy.Filesystem, stat *gonfs.FSStat) error {
	space := h.fs.space(ctx)
	stat.TotalSize = space.Total
	stat.FreeSize = space.Free
	stat.AvailableSize = space.Free
	return nil
}

// ToHandle returns the file handle of a path and remembers it.
func (h *handler) ToHandle(_ billy.Filesystem, p []string) []byte {
	joined := strings.Join(p, "/")
	key := handleKey(sha256.Sum256([]byte(joined)))
	h.handles.add(key, joined)
	return key[:]
}

// FromHandle returns the path a handle stands for, or NFS3ERR_STALE when the handle is
// unknown, which the client answers by looking the name up again.
//
//nolint:ireturn // The signature is fixed by gonfs.Handler.
func (h *handler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	if len(fh) == len(handleKey{}) {
		if joined, ok := h.handles.get(handleKey(fh)); ok {
			return h.fs, splitPath(joined), nil
		}
	}
	return nil, nil, &gonfs.NFSStatusError{NFSStatus: gonfs.NFSStatusStale}
}

// splitPath returns a new slice each time because go-nfs appends to the result.
func splitPath(joined string) []string {
	if joined == "" {
		return []string{}
	}
	return strings.Split(joined, "/")
}

// InvalidateHandle forgets a handle. go-nfs calls it after removing a file, which this
// read-only mount never does, but the interface requires it.
func (h *handler) InvalidateHandle(_ billy.Filesystem, fh []byte) error {
	if len(fh) == len(handleKey{}) {
		h.handles.remove(handleKey(fh))
	}
	return nil
}

// HandleLimit is how many handles are kept. go-nfs also uses half of it as the most
// entries it puts in one READDIR reply.
func (h *handler) HandleLimit() int {
	return h.handleLimit
}

// VerifierFor remembers a directory listing and returns its cookie verifier. The
// verifier is derived from the path and the names, so it changes when the directory
// changes, and it is never 0, which NFS reserves for "no verifier".
func (h *handler) VerifierFor(dirPath string, contents []fs.FileInfo) uint64 {
	hashed := binary.BigEndian.AppendUint64(nil, uint64(len(dirPath)))
	hashed = append(hashed, dirPath...)
	for _, entry := range contents {
		hashed = append(hashed, entry.Name()...)
		hashed = append(hashed, 0)
	}
	sum := sha256.Sum256(hashed)
	id := reserveZero(binary.BigEndian.Uint64(sum[:8]))
	h.verifiers.add(id, listing{path: dirPath, entries: contents})
	return id
}

// reserveZero maps the one hash value that NFS reserves for "no verifier" to another.
func reserveZero(id uint64) uint64 {
	if id == 0 {
		return 1
	}
	return id
}

// DataForVerifier returns the listing remembered for a verifier, or nil.
func (h *handler) DataForVerifier(dirPath string, verifier uint64) []fs.FileInfo {
	remembered, ok := h.verifiers.get(verifier)
	if !ok || remembered.path != dirPath {
		return nil
	}
	return remembered.entries
}
