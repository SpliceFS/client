package core

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/tree"
)

// view is the FS of one mount: a directory of a cluster, read with the mount's cache
// settings.
type view struct {
	client *Client
	c      *cluster
	prefix string // the mount's directory inside the cluster, "" for its root
	mount  config.Mount
	cache  *cacheDir
	space  spaceMemo

	released bool // Release was called; guarded by the Client's mu
}

// Stat describes the file or directory at name.
func (v *view) Stat(ctx context.Context, name string) (Attr, error) {
	treeEntry, err := v.lookup(ctx, "stat", name)
	if err != nil {
		return Attr{}, err
	}
	return v.attr(name, treeEntry), nil
}

// ReadDir lists the directory at name, sorted by name, without the names desktop
// systems probe for.
func (v *view) ReadDir(ctx context.Context, name string) ([]Attr, error) {
	const operation = "read directory"
	if refused(name) {
		return nil, notExist(operation, name)
	}
	entries, err := v.c.tree.List(ctx, v.full(name))
	if err != nil {
		return nil, fail(ctx, operation, name, err)
	}
	attrs := make([]Attr, 0, len(entries))
	for _, treeEntry := range entries {
		if !refusedSegment(treeEntry.Name) {
			attrs = append(attrs, v.attr(join(name, treeEntry.Name), treeEntry))
		}
	}
	return attrs, nil
}

// lookup is the tree's entry for name, with the errors of FS.
func (v *view) lookup(ctx context.Context, operation, name string) (tree.Entry, error) {
	if refused(name) {
		return tree.Entry{}, notExist(operation, name)
	}
	treeEntry, err := v.c.tree.Stat(ctx, v.full(name))
	if err != nil {
		return tree.Entry{}, fail(ctx, operation, name, err)
	}
	return treeEntry, nil
}

func (v *view) attr(name string, treeEntry tree.Entry) Attr {
	return Attr{
		Name:  name[strings.LastIndexByte(name, '/')+1:],
		Dir:   treeEntry.Dir,
		Size:  treeEntry.Size,
		MTime: treeEntry.MTime,
		Inode: inode(v.c.name, v.full(name), kindOf(treeEntry)),
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
func fail(ctx context.Context, operation, name string, err error) error {
	switch {
	case errors.Is(err, tree.ErrNotFound):
		return notExist(operation, name)
	case errors.Is(err, tree.ErrNotDir):
		return fmt.Errorf("%s %q: %w", operation, name, ErrNotDir)
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	}
	return fmt.Errorf("%s %q: %w: %w", operation, name, ErrUnavailable, err)
}

func notExist(operation, name string) error {
	return fmt.Errorf("%s %q: %w", operation, name, ErrNotExist)
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

// kind is the byte that tells a file from a directory in an inode number.
type kind byte

const (
	kindFile kind = 'f'
	kindDir  kind = 'd'
)

func kindOf(treeEntry tree.Entry) kind {
	if treeEntry.Dir {
		return kindDir
	}
	return kindFile
}

// inode is the number of a path inside a cluster. It depends on nothing but the
// cluster name, the path and its kind, so it is the same in every view and after a
// restart. The kind is part of it because a mount layer keeps one kernel inode per
// number: a name that turns from a file into a directory must not reuse the number.
func inode(clusterName, path string, entryKind kind) uint64 {
	hasher := fnv.New64a()
	// Writes to a hash never fail.
	_, _ = hasher.Write([]byte(clusterName))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(path))
	_, _ = hasher.Write([]byte{0, byte(entryKind)})
	return remapReserved(hasher.Sum64())
}

// inodeRemapOffset moves a reserved number to the first number after the reserved ones.
const inodeRemapOffset = 2

func remapReserved(ino uint64) uint64 {
	switch ino {
	case inodeNone, inodeRoot:
		return ino + inodeRemapOffset
	}
	return ino
}
