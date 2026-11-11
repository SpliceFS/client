package core

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/BlueRival/splicefs/client/config"
	"github.com/BlueRival/splicefs/client/internal/cache"
	"github.com/BlueRival/splicefs/client/internal/tree"
)

// view is the FS of one mount: a directory of a cluster, read with the mount's cache
// settings.
type view struct {
	client *Client
	c      *cluster
	prefix string // the mount's directory inside the cluster, "" for its root
	mount  config.Mount
	cache  *cache.Cache
	space  spaceMemo
}

// Stat describes the file or directory at name.
func (v *view) Stat(ctx context.Context, name string) (Attr, error) {
	e, err := v.stat(ctx, "stat", name)
	if err != nil {
		return Attr{}, err
	}
	return v.attr(name, e), nil
}

// ReadDir lists the directory at name, sorted by name, without the names desktop
// systems probe for.
func (v *view) ReadDir(ctx context.Context, name string) ([]Attr, error) {
	const op = "read directory"
	if refused(name) {
		return nil, notExist(op, name)
	}
	entries, err := v.c.tree.List(ctx, v.full(name))
	if err != nil {
		return nil, v.fail(ctx, op, name, err)
	}
	attrs := make([]Attr, 0, len(entries))
	for _, e := range entries {
		if !refusedSegment(e.Name) {
			attrs = append(attrs, v.attr(join(name, e.Name), e))
		}
	}
	return attrs, nil
}

func (v *view) stat(ctx context.Context, op, name string) (tree.Entry, error) {
	if refused(name) {
		return tree.Entry{}, notExist(op, name)
	}
	e, err := v.c.tree.Stat(ctx, v.full(name))
	if err != nil {
		return tree.Entry{}, v.fail(ctx, op, name, err)
	}
	return e, nil
}

func (v *view) attr(name string, e tree.Entry) Attr {
	return Attr{
		Name:  name[strings.LastIndexByte(name, '/')+1:],
		Dir:   e.Dir,
		Size:  e.Size,
		MTime: e.MTime,
		Inode: inode(v.c.name, v.full(name)),
	}
}

// full turns a name in the view into the path inside the cluster.
func (v *view) full(name string) string {
	switch {
	case v.prefix == "":
		return name
	case name == "":
		return v.prefix
	}
	return v.prefix + "/" + name
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// fail translates an error of the tree or the session into the errors of FS. The
// caller's own context ending is passed through as it is, so that an interrupted
// call is not mistaken for an outage.
func (v *view) fail(ctx context.Context, op, name string, err error) error {
	switch {
	case errors.Is(err, tree.ErrNotFound):
		return notExist(op, name)
	case errors.Is(err, tree.ErrNotDir):
		return fmt.Errorf("%s %q: %w", op, name, ErrNotDir)
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	}
	return fmt.Errorf("%s %q: %w: %w", op, name, ErrUnavailable, err)
}

func notExist(op, name string) error {
	return fmt.Errorf("%s %q: %w", op, name, ErrNotExist)
}

// refused reports whether any segment of name is one that desktop systems probe for.
// Such names are answered at once: Finder and Spotlight ask for them in every
// directory they see, and each question would otherwise cost a listing on every node.
func refused(name string) bool {
	for segment := range strings.SplitSeq(name, "/") {
		if refusedSegment(segment) {
			return true
		}
	}
	return false
}

func refusedSegment(name string) bool {
	switch name {
	case ".DS_Store", ".hidden", ".localized", ".metadata_never_index", ".Spotlight-V100",
		".fseventsd", ".TemporaryItems", ".apdisk", ".VolumeIcon.icns":
		return true
	}
	return strings.HasPrefix(name, "._") || strings.HasPrefix(name, ".Trash")
}

// Inode numbers that mount layers reserve: 0 means no inode, and FUSE gives 1 to the
// root of the mount.
const (
	inodeNone = 0
	inodeRoot = 1
)

// inode is the number of a path inside a cluster. It depends on nothing but the
// cluster name and the path, so it is the same in every view and after a restart.
func inode(clusterName, path string) uint64 {
	h := fnv.New64a()
	// Writes to a hash never fail.
	_, _ = h.Write([]byte(clusterName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(path))
	return remapReserved(h.Sum64())
}

func remapReserved(ino uint64) uint64 {
	switch ino {
	case inodeNone, inodeRoot:
		return ino + 2
	}
	return ino
}
