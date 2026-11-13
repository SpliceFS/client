//go:build linux

package fuse

import (
	"syscall"
	"testing"
	"time"
)

// fuseMagic is the file system type number the Linux kernel reports for every FUSE mount.
const fuseMagic = 0x65735546

// fileType is the file type bits of a mode. The mode is a uint32 here and a uint16 on
// FreeBSD, which is why the shared tests cannot do the masking themselves.
func fileType(st *syscall.Stat_t) uint32 { return st.Mode & syscall.S_IFMT }

// accessTime and changeTime are named Atim and Ctim in a Linux Stat_t, and Atimespec and
// Ctimespec on FreeBSD.
func accessTime(st *syscall.Stat_t) time.Time { return time.Unix(st.Atim.Sec, st.Atim.Nsec) }

func changeTime(st *syscall.Stat_t) time.Time { return time.Unix(st.Ctim.Sec, st.Ctim.Nsec) }

// isFuseStatfs reports whether a statfs answer describes a FUSE mount. Linux identifies the
// file system by a fixed magic number.
func isFuseStatfs(st *syscall.Statfs_t) bool { return st.Type == fuseMagic }

// isMountPoint reports whether target is a mount point, as the mount table says.
func isMountPoint(t *testing.T, target string) bool {
	t.Helper()
	_, ok := findMount(t, target)
	return ok
}
