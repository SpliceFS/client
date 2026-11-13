package cache

import (
	"errors"
	"testing"
)

func TestVersionValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    Version
		ok   bool
	}{
		{"valid", testVersion(10), true},
		{"empty file", testVersion(0), true},
		{"zero block size", Version{Size: 10}, false},
		{"negative size", Version{Size: -1, BlockSize: 4}, false},
		{"too many blocks", Version{Size: maxBlocks + 1, BlockSize: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.v.validate()
			if tt.ok != (err == nil) {
				t.Fatalf("validate() = %v, want ok=%v", err, tt.ok)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidVersion) {
				t.Errorf("error %v is not ErrInvalidVersion", err)
			}
		})
	}
}

func TestVersionBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		size, blocks int64
	}{{0, 0}, {1, 1}, {4, 1}, {5, 2}, {8, 2}, {10, 3}}
	for _, tt := range tests {
		if got := testVersion(tt.size).blocks(); got != tt.blocks {
			t.Errorf("size %d: blocks() = %d, want %d", tt.size, got, tt.blocks)
		}
	}
}

func TestVersionBlockLen(t *testing.T) {
	t.Parallel()
	v := testVersion(10)
	tests := []struct {
		block, want int64
	}{{-1, -1}, {0, 4}, {1, 4}, {2, 2}, {3, -1}}
	for _, tt := range tests {
		if got := v.blockLen(tt.block); got != tt.want {
			t.Errorf("blockLen(%d) = %d, want %d", tt.block, got, tt.want)
		}
	}
}

func TestCheckBitmap(t *testing.T) {
	t.Parallel()
	version := testVersion(40) // 10 blocks, 2 bitmap bytes
	tests := []struct {
		name   string
		bitmap []byte
		ok     bool
	}{
		{"valid", []byte{0xff, 0x03}, true},
		{"too short", []byte{0xff}, false},
		{"too long", []byte{0, 0, 0}, false},
		{"bits past the end", []byte{0, 0x04}, false},
	}
	for _, tt := range tests {
		if err := checkBitmap(version, tt.bitmap); tt.ok != (err == nil) {
			t.Errorf("%s: checkBitmap() = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
	if err := checkBitmap(testVersion(32), []byte{0xff}); err != nil {
		t.Errorf("full last byte of an exact multiple: %v", err)
	}
}

func TestCountBlocks(t *testing.T) {
	t.Parallel()
	version := testVersion(10) // blocks of 4, 4, 2
	tests := []struct {
		name          string
		bitmap        []byte
		present, size int64
	}{
		{"none", []byte{0}, 0, 0},
		{"first", []byte{1}, 1, 4},
		{"short last", []byte{4}, 1, 2},
		{"all", []byte{7}, 3, 10},
	}
	for _, tt := range tests {
		got := countBlocks(version, tt.bitmap)
		if got.present != tt.present || got.bytes != tt.size {
			t.Errorf("%s: countBlocks() = %d, %d; want %d, %d", tt.name, got.present, got.bytes, tt.present, tt.size)
		}
	}
	if got := countBlocks(testVersion(0), nil); got != (blockCount{}) {
		t.Errorf("empty file: countBlocks() = %+v", got)
	}
}

func TestBaseNameIsSafeAndDistinct(t *testing.T) {
	t.Parallel()
	a := baseName("/cache", Key{Cluster: "a", Path: "/../../etc/passwd"})
	b := baseName("/cache", Key{Cluster: "a/", Path: "../../etc/passwd"})
	if a == b {
		t.Errorf("different keys share a name: %s", a)
	}
	if !isLowerHex(hashName(Key{Path: "x"}), hashLen) {
		t.Error("hash is not 64 lowercase hex digits")
	}
}
