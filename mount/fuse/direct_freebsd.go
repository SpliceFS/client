//go:build freebsd

package fuse

import "github.com/hanwen/go-fuse/v2/fuse"

// systemOptions adds what FreeBSD needs. FreeBSD has no mount(2) route for FUSE: go-fuse
// always mounts through mount_fusefs, which takes the read-only setting as the option "ro"
// that is already there. FreeBSD's FUSE keeps the access time of a file in the kernel and
// moves it at every read, even on a read-only mount, so the mount says noatime: the times
// of a file are the ones the servers report, and reading must not change them.
func systemOptions(o *fuse.MountOptions) {
	o.Options = append(o.Options, "noatime")
}
