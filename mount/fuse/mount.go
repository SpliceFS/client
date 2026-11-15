//go:build linux || freebsd

package fuse

import (
	"context"
	"fmt"
	stdfs "io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

const (
	// subtype is shown after "fuse." as the filesystem type in mount tables.
	subtype = "splicefs"
	// setupTimeout bounds the removal of a dead mount before a new mount is made.
	setupTimeout = 30 * time.Second
)

// Mount serves fsys read-only on opts.Target and returns once the mount is ready.
//
// The target must be an existing directory. A dead mount left on it by a crashed client
// is detached first. The mount is made with the mount system call when the process is
// allowed to, and otherwise through the setuid fusermount3 helper.
//
//nolint:ireturn // mount.Mounted is the result type of every mount layer.
func Mount(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
	return mountWith(fsys, opts, defaultSettings(), newKernelHost())
}

//nolint:ireturn // mount.Mounted is the result type of every mount layer.
func mountWith(fsys core.FS, opts mount.Options, tunables settings, sys host) (mount.Mounted, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := prepareTarget(ctx, sys, opts.Target); err != nil {
		return nil, err
	}

	log := loggerOrDiscard(opts.Logger)
	files := newOpenFiles()
	root := &dirNode{node{env: &env{
		fsys:   fsys,
		owner:  currentOwner(),
		mapper: newErrorMapper(log),
		files:  files,
	}}}
	srv, err := fuse.NewServer(fs.NewNodeFS(root, nodeOptions(tunables, log)), opts.Target, mountOptions(opts, log))
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w", opts.Target, err)
	}
	return start(server{Server: srv, files: files}, sys, opts.Target)
}

// statPath is os.Stat with the failure wrapped, for the host implementations to share.
func statPath(path string) (stdfs.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("check path: %w", err)
	}
	return info, nil
}

// currentOwner is the user running the client, who owns every file of the mount.
func currentOwner() owner {
	o := fuse.CurrentOwner()
	return owner{uid: o.Uid, gid: o.Gid}
}

func loggerOrDiscard(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}

func nodeOptions(s settings, log *slog.Logger) *fs.Options {
	return &fs.Options{
		EntryTimeout:    &s.entryTimeout,
		AttrTimeout:     &s.attrTimeout,
		NegativeTimeout: &s.negativeTimeout,
		Logger:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// mountOptions describes the mount to the kernel. Read-only makes the kernel refuse
// writes itself. The system specific part is in systemOptions.
func mountOptions(opts mount.Options, log *slog.Logger) *fuse.MountOptions {
	kernelOptions := &fuse.MountOptions{
		FsName:     opts.Source,
		Name:       subtype,
		AllowOther: opts.AllowOther,
		Options:    []string{"ro"},
		Logger:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	systemOptions(kernelOptions)
	return kernelOptions
}
