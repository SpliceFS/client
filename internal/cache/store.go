package cache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const (
	dataExt  = ".data"
	metaExt  = ".meta"
	tmpExt   = ".meta.tmp"
	shardLen = 2
)

// snapshot is a consistent copy of an entry's metadata, taken under Cache.mu.
type snapshot struct {
	entry *entry
	meta  metaFile
	seq   uint64
	// f is the data file handle at the time of the snapshot, or nil if the entry was
	// not pinned. Everything the bitmap claims was written through it before the
	// snapshot, so syncing it makes the claim true on disk.
	f dataFile
}

func (e *entry) snapshotLocked() snapshot {
	e.seq++
	e.fresh = 0
	return snapshot{
		entry: e,
		seq:   e.seq,
		f:     e.f,
		meta: metaFile{
			Key:       e.key,
			Version:   e.version,
			Blocks:    bytes.Clone(e.bitmap),
			Validated: e.validated,
			LastRead:  e.lastRead,
		},
	}
}

// write persists the snapshot: sync the data file, then replace the metadata file
// atomically. The data is synced after the snapshot was taken and before the metadata
// names its blocks, so the bitmap on disk never claims a block that is not on disk.
// A snapshot older than one already written, or of a removed entry, is skipped.
func (s snapshot) write(fsys fileSystem) error {
	e := s.entry
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.removed || s.seq <= e.persisted {
		return nil
	}
	if s.f != nil {
		if err := s.f.Sync(); err != nil {
			return fmt.Errorf("sync data file: %w", err)
		}
	}
	data, err := json.Marshal(s.meta)
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	if err := writeAtomic(fsys, e.base+metaExt, e.base+tmpExt, data); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	e.persisted = s.seq
	return nil
}

// writeAtomic replaces path with data. The temporary file is synced before the rename so
// that a crash leaves either the old or the new content, never a torn file.
func writeAtomic(fsys fileSystem, path, tmp string, data []byte) error {
	f, err := fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err = f.WriteAt(data, 0); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsys.Rename(tmp, path)
	}
	if err != nil {
		return errors.Join(err, unlink(fsys, tmp))
	}
	return nil
}

// unlink removes a file, treating a file that is already gone as success.
func unlink(fsys fileSystem, name string) error {
	if err := fsys.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// removeLocked drops the entry and unlinks its files. Holders that still have the data
// file open keep working on it; the unlinked inode lives until they close it. The entry
// is dropped even if unlinking fails, and the leftovers are cleaned up by the next Open.
func (c *Cache) removeLocked(e *entry) error {
	delete(c.entries, e.key)
	c.total -= e.bytes
	e.pmu.Lock()
	defer e.pmu.Unlock()
	e.removed = true
	return errors.Join(unlink(c.fs, e.base+dataExt), unlink(c.fs, e.base+metaExt))
}
