package fuse

import (
	"syscall"
	"testing"
	"time"

	"github.com/BlueRival/splicefs/client/core"
)

func TestNewAttributes(t *testing.T) {
	t.Parallel()
	mtime := time.Unix(1_700_000_000, 123_456_789)
	o := owner{uid: 1000, gid: 1001}
	tests := []struct {
		name string
		in   core.Attr
		want attributes
	}{
		{
			name: "file",
			in:   core.Attr{Name: "a", Size: 1000, MTime: mtime, Inode: 42},
			want: attributes{
				ino: 42, mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o,
				size: 1000, blocks: 2, mtimeSec: 1_700_000_000, mtimeNsec: 123_456_789,
			},
		},
		{
			name: "directory",
			in:   core.Attr{Name: "d", Dir: true, MTime: mtime, Inode: 7},
			want: attributes{
				ino: 7, mode: syscall.S_IFDIR | 0o555, nlink: 1, owner: o,
				mtimeSec: 1_700_000_000, mtimeNsec: 123_456_789,
			},
		},
		{
			name: "blocks round up in 512 byte units",
			in:   core.Attr{Size: 513, MTime: mtime},
			want: attributes{
				mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o,
				size: 513, blocks: 2, mtimeSec: 1_700_000_000, mtimeNsec: 123_456_789,
			},
		},
		{
			name: "exact multiple of 512",
			in:   core.Attr{Size: 1024, MTime: mtime},
			want: attributes{
				mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o,
				size: 1024, blocks: 2, mtimeSec: 1_700_000_000, mtimeNsec: 123_456_789,
			},
		},
		{
			name: "negative size is empty",
			in:   core.Attr{Size: -5, MTime: mtime},
			want: attributes{
				mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o,
				mtimeSec: 1_700_000_000, mtimeNsec: 123_456_789,
			},
		},
		{
			name: "zero time becomes the epoch",
			in:   core.Attr{Size: 1},
			want: attributes{mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o, size: 1, blocks: 1},
		},
		{
			name: "time before the epoch becomes the epoch",
			in:   core.Attr{MTime: time.Unix(-10, 5)},
			want: attributes{mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o},
		},
		{
			name: "the epoch itself",
			in:   core.Attr{MTime: time.Unix(0, 9)},
			want: attributes{mode: syscall.S_IFREG | 0o444, nlink: 1, owner: o, mtimeNsec: 9},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := newAttributes(tt.in, o); got != tt.want {
				t.Fatalf("newAttributes(%+v) =\n%+v\nwant\n%+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCapacityOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   core.Space
		want capacity
	}{
		{"whole blocks", core.Space{Total: 10 * blockSize, Free: 3 * blockSize}, capacity{blocks: 10, free: 3}},
		{"partial blocks round down", core.Space{Total: 10*blockSize + 100, Free: 3*blockSize + 100}, capacity{blocks: 10, free: 3}},
		{"free larger than total is clamped", core.Space{Total: 5 * blockSize, Free: 9 * blockSize}, capacity{blocks: 5, free: 5}},
		{"empty", core.Space{}, capacity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := capacityOf(tt.in); got != tt.want {
				t.Fatalf("capacityOf(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
