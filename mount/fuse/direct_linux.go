//go:build linux

package fuse

import (
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// systemOptions makes go-fuse call mount(2) first, which needs no helper program when the
// client runs as root, and fall back to fusermount3 otherwise. The two routes take the
// read-only setting differently: mount(2) as a flag, which go-fuse replaces with its own
// default unless it is given again here, and fusermount3 as the option "ro".
func systemOptions(o *fuse.MountOptions) {
	o.DirectMount = true
	o.DirectMountFlags = syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV
}
