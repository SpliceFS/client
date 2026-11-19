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
	// present is how many blocks meta names. Blocks are only ever added to an entry, so
	// a count above the one on disk means the snapshot records new blocks.
	present int64
	// f is the data file handle at the time of the snapshot, or nil if the entry was
	// not pinned. Everything the bitmap claims was written through it before the
	// snapshot, so syncing it makes the claim true on disk.
	f dataFile
}

func (e *entry) snapshotLocked() snapshot {
	e.seq++
	e.fresh = 0
	return snapshot{
		entry:   e,
		seq:     e.seq,
		f:       e.f,
		present: e.present,
		meta: metaFile{
			Key:       e.key,
			Version:   e.version,
			Blocks:    bytes.Clone(e.bitmap),
			Validated: e.validated,
			LastRead:  e.lastRead,
		},
	}
}

// write persists the snapshot. A snapshot that records new blocks syncs the data file
// first and then replaces the metadata file durably, so the bitmap on disk never claims a
// block that is not on disk. A snapshot that changes only times replaces the metadata file
// without syncing anything: a crash can lose it or leave it empty, which costs one
// revalidation or an older eviction order, never wrong data, because an unreadable
// metadata file drops the entry when the cache is opened.
// A snapshot older than one already written, or of a removed entry, is skipped.
func (s snapshot) write(fsys fileSystem) error {
	cached := s.entry
	cached.pmu.Lock()
	defer cached.pmu.Unlock()
	if cached.removed || s.seq <= cached.persisted {
		return nil
	}
	data, err := json.Marshal(s.meta)
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	finish := skipSync
	if s.present > cached.persistedBlocks {
		if err := s.syncData(); err != nil {
			return err
		}
		finish = dataFile.Sync
	}
	if err := replaceFile(fsys, cached.base+metaExt, cached.base+tmpExt, data, finish); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	cached.persisted = s.seq
	cached.persistedBlocks = s.present
	return nil
}

// syncData makes the blocks the snapshot names durable.
func (s snapshot) syncData() error {
	if s.f == nil {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("sync data file: %w", err)
	}
	return nil
}

func skipSync(dataFile) error { return nil }

// replaceFile replaces path with data through a temporary file and a rename, so readers
// see the old or the new content. finish runs on the temporary file before the rename;
// syncing there makes the replacement survive a crash whole.
func replaceFile(fsys fileSystem, path, tmp string, data []byte, finish func(dataFile) error) error {
	file, err := fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	if _, err = file.WriteAt(data, 0); err == nil {
		err = finish(file)
	}
	if cerr := file.Close(); err == nil {
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
func (c *Cache) removeLocked(cached *entry) error {
	delete(c.entries, cached.key)
	c.total -= cached.bytes
	cached.pmu.Lock()
	defer cached.pmu.Unlock()
	cached.removed = true
	return errors.Join(unlink(c.fs, cached.base+dataExt), unlink(c.fs, cached.base+metaExt))
}
