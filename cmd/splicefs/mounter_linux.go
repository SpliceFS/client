//go:build linux

package main

import (
	"github.com/BlueRival/splicefs/client/app"
	"github.com/BlueRival/splicefs/client/mount/fuse"
)

// platformMounter mounts through FUSE.
func platformMounter() app.Mounter { return fuse.Mount }
