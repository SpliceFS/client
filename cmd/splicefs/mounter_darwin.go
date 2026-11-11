//go:build darwin

package main

import (
	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/mount/nfs"
)

// platformMounter mounts through a local NFS server and the NFS client of macOS.
func platformMounter() app.Mounter { return nfs.Mount }
