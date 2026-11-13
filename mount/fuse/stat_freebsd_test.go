//go:build freebsd

package fuse

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mntNoWait is MNT_NOWAIT from sys/mount.h; the syscall package does not export it for
// FreeBSD. It makes getfsstat answer from cached data instead of asking every file system,
// which a FUSE mount that is being torn down could answer slowly.
const mntNoWait = 2

// fileType is the file type bits of a mode. The mode is a uint16 here and a uint32 on
// Linux, which is why the shared tests cannot do the masking themselves.
func fileType(st *syscall.Stat_t) uint32 { return uint32(st.Mode) & syscall.S_IFMT }

// accessTime and changeTime are named Atimespec and Ctimespec in a FreeBSD Stat_t, and Atim
// and Ctim on Linux.
func accessTime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
}

func changeTime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}

// isFuseStatfs reports whether a statfs answer describes a FUSE mount. FreeBSD hands out
// file system type numbers dynamically, so the type name is the stable identification. The
// name carries the subtype of the mount after a dot: fusefs.splicefs.
func isFuseStatfs(st *syscall.Statfs_t) bool {
	name := cString(st.Fstypename[:])
	return name == "fusefs" || strings.HasPrefix(name, "fusefs.")
}

// isMountPoint reports whether target is a mount point, as the kernel's list of mounted
// file systems says. FreeBSD has no mount table file to read.
func isMountPoint(t *testing.T, target string) bool {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	count, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil {
		t.Fatal(err)
	}
	list := make([]syscall.Statfs_t, count)
	count, err = syscall.Getfsstat(list, mntNoWait)
	if err != nil {
		t.Fatal(err)
	}
	for i := range list[:count] {
		if cString(list[i].Mntonname[:]) == resolved {
			return true
		}
	}
	return false
}

// cString reads a NUL terminated C string out of a fixed size array.
func cString(chars []int8) string {
	out := make([]byte, 0, len(chars))
	for _, c := range chars {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
