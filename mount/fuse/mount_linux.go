//go:build linux

package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"syscall"
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
func Mount(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
	return mountWith(fsys, opts, defaultSettings(), newKernelHost())
}

func mountWith(fsys core.FS, opts mount.Options, s settings, h host) (mount.Mounted, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	if err := prepareTarget(ctx, h, opts.Target); err != nil {
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
	srv, err := fuse.NewServer(fs.NewNodeFS(root, nodeOptions(s, log)), opts.Target, mountOptions(opts, log))
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w", opts.Target, err)
	}
	return start(server{Server: srv, files: files}, h, opts.Target)
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
// writes itself. DirectMount makes go-fuse call mount(2) first, which needs no helper
// program when the client runs as root, and fall back to fusermount3 otherwise. The two
// routes take the read-only setting differently: mount(2) as a flag, which go-fuse
// replaces with its own default unless it is given again here, and fusermount3 as the
// option "ro".
func mountOptions(opts mount.Options, log *slog.Logger) *fuse.MountOptions {
	return &fuse.MountOptions{
		FsName:           opts.Source,
		Name:             subtype,
		AllowOther:       opts.AllowOther,
		Options:          []string{"ro"},
		DirectMount:      true,
		DirectMountFlags: syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV,
		Logger:           slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
