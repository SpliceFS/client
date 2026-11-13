package fuse

import (
	"syscall"
	"time"

	"github.com/SpliceFS/client/core"
)

const (
	// blockSize is the block size reported by statfs.
	blockSize = 4096
	// sectorSize is the unit of st_blocks, fixed by POSIX practice at 512 bytes.
	sectorSize = 512
	fileMode   = 0o444
	dirMode    = 0o555
)

// owner is the user and group that own every file of a mount.
type owner struct {
	uid, gid uint32
}

// attributes is the stat information of one file or directory as the kernel will see
// it. It holds plain numbers so that the conversion from core.Attr does not depend on
// go-fuse.
type attributes struct {
	ino       uint64
	mode      uint32 // file type and permission bits
	nlink     uint32
	owner     owner
	size      uint64
	blocks    uint64
	mtimeSec  uint64
	mtimeNsec uint32
}

// newAttributes converts the description of a file from the core into what the kernel
// is told: read-only permissions, one link, the given owner, and one timestamp that
// serves as modification, access and change time alike.
func newAttributes(attr core.Attr, fileOwner owner) attributes {
	mode := uint32(syscall.S_IFREG | fileMode)
	if attr.Dir {
		mode = syscall.S_IFDIR | dirMode
	}
	size := uint64(max(attr.Size, 0))
	mtime := unixTime(attr.MTime)
	return attributes{
		ino:       attr.Inode,
		mode:      mode,
		nlink:     1,
		owner:     fileOwner,
		size:      size,
		blocks:    (size + sectorSize - 1) / sectorSize,
		mtimeSec:  mtime.sec,
		mtimeNsec: mtime.nsec,
	}
}

// epochTime is a point in time as the kernel structure holds it.
type epochTime struct {
	sec  uint64
	nsec uint32
}

// unixTime splits t into seconds and nanoseconds since the epoch. Times before the
// epoch, including the zero time of a node that reported none, become the epoch
// because the kernel structure cannot hold a negative time.
func unixTime(t time.Time) epochTime {
	if t.Before(time.Unix(0, 0)) {
		return epochTime{}
	}
	// t is not before the epoch, so both values are non-negative.
	return epochTime{sec: uint64(t.Unix()), nsec: uint32(t.Nanosecond())} //nolint:gosec // see above
}

// capacity is the size of a view in statfs blocks.
type capacity struct {
	blocks uint64
	free   uint64
}

// capacityOf converts a byte count into blocks of blockSize. Free space is never
// reported as larger than the total, which would make df show negative usage.
func capacityOf(s core.Space) capacity {
	return capacity{
		blocks: s.Total / blockSize,
		free:   min(s.Free, s.Total) / blockSize,
	}
}
