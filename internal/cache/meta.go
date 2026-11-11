package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/bits"
	"path/filepath"
	"time"
)

// maxBlocks bounds the bitmap of one entry, so a bogus size from the network cannot
// make the cache allocate without limit.
const maxBlocks = 1 << 24

// Key identifies a remote file: the cluster it belongs to and its path in the cluster.
type Key struct {
	Cluster string `json:"cluster"`
	Path    string `json:"path"`
}

// Version identifies a particular version of a remote file and how it is divided into
// blocks. Two versions are the same only if every field is equal.
type Version struct {
	Size       int64  `json:"size"`
	ETag       string `json:"etag"`
	HeadDigest string `json:"head_digest"`
	BlockSize  int64  `json:"block_size"`
}

func (v Version) validate() error {
	switch {
	case v.BlockSize <= 0:
		return fmt.Errorf("%w: block size %d", ErrInvalidVersion, v.BlockSize)
	case v.Size < 0:
		return fmt.Errorf("%w: size %d", ErrInvalidVersion, v.Size)
	case v.blocks() > maxBlocks:
		return fmt.Errorf("%w: %d blocks of %d bytes", ErrInvalidVersion, v.blocks(), v.BlockSize)
	}
	return nil
}

// blocks is the number of blocks in the file; the last one may be short.
func (v Version) blocks() int64 {
	n := v.Size / v.BlockSize
	if v.Size%v.BlockSize != 0 {
		n++
	}
	return n
}

// blockLen is the length of block, or -1 if the block is out of range.
func (v Version) blockLen(block int64) int64 {
	if block < 0 || block >= v.blocks() {
		return -1
	}
	return min(v.BlockSize, v.Size-block*v.BlockSize)
}

// metaFile is the JSON form of an entry's metadata.
type metaFile struct {
	Key       Key       `json:"key"`
	Version   Version   `json:"version"`
	Blocks    []byte    `json:"blocks"`
	Validated time.Time `json:"validated"`
	LastRead  time.Time `json:"last_read"`
}

// bitmapLen is the number of bytes of a bitmap for n blocks.
func bitmapLen(n int64) int64 { return (n + 7) / 8 }

// checkBitmap verifies that a stored bitmap fits the version: the right length and no
// bits set beyond the last block.
func checkBitmap(v Version, bitmap []byte) error {
	n := v.blocks()
	if int64(len(bitmap)) != bitmapLen(n) {
		return fmt.Errorf("bitmap is %d bytes, want %d", len(bitmap), bitmapLen(n))
	}
	if extra := n % 8; extra != 0 && bitmap[len(bitmap)-1]>>extra != 0 {
		return fmt.Errorf("bitmap has blocks beyond the end of the file")
	}
	return nil
}

// countBlocks returns how many blocks the bitmap marks and how many bytes they hold.
func countBlocks(v Version, bitmap []byte) (present, size int64) {
	for _, b := range bitmap {
		present += int64(bits.OnesCount8(b))
	}
	size = present * v.BlockSize
	if last := v.blocks() - 1; last >= 0 && hasBit(bitmap, last) {
		size -= v.BlockSize - v.blockLen(last)
	}
	return present, size
}

func hasBit(bitmap []byte, i int64) bool { return bitmap[i/8]&(1<<(i%8)) != 0 }

func setBit(bitmap []byte, i int64) { bitmap[i/8] |= 1 << (i % 8) }

// baseName is the path, without extension, of the files holding key. It is derived from
// a hash so that any cluster name and path is safe as a file name. The first two hex
// digits name a subdirectory to keep directories small.
func baseName(dir string, key Key) string {
	h := hashName(key)
	return filepath.Join(dir, h[:shardLen], h)
}

func hashName(key Key) string {
	sum := sha256.Sum256([]byte(key.Cluster + "\x00" + key.Path))
	return hex.EncodeToString(sum[:])
}
