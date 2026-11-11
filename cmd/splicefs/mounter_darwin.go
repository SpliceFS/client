//go:build darwin

package main

import (
	"github.com/BlueRival/splicefs/client/app"
	"github.com/BlueRival/splicefs/client/mount/nfs"
)

// platformMounter mounts through a local NFS server and the NFS client of macOS.
func platformMounter() app.Mounter { return nfs.Mount }
