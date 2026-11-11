//go:build linux

package main

import (
	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/mount/fuse"
)

// platformMounter mounts through FUSE.
func platformMounter() app.Mounter { return fuse.Mount }
