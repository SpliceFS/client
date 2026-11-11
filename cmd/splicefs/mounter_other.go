//go:build !linux && !darwin

package main

import (
	"errors"

	"github.com/BlueRival/splicefs/client/app"
	"github.com/BlueRival/splicefs/client/core"
	"github.com/BlueRival/splicefs/client/mount"
)

// platformMounter refuses every mount: there is no mount layer for this system.
func platformMounter() app.Mounter {
	return func(core.FS, mount.Options) (mount.Mounted, error) {
		return nil, errors.New("mounting is not supported on this operating system")
	}
}
