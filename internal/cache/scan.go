package cache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// hashLen is the length of a hex SHA-256, which is the stem of every file the cache creates.
const hashLen = 64

// files is the set of files found for one hash while scanning.
type files struct {
	data, meta, tmp bool
}

// scan loads the entries found in the cache directory and removes the files the cache
// would have created that cannot be used. Files it does not recognize are left alone.
func (c *Cache) scan() error {
	shards, err := c.fs.ReadDir(c.opts.Dir)
	if err != nil {
		return fmt.Errorf("scan cache directory: %w", err)
	}
	for _, s := range shards {
		if s.IsDir() && isLowerHex(s.Name(), shardLen) {
			c.scanShard(s.Name())
		}
	}
	return nil
}

func (c *Cache) scanShard(shard string) {
	dir := filepath.Join(c.opts.Dir, shard)
	infos, err := c.fs.ReadDir(dir)
	if err != nil {
		c.log.Warn("scan cache subdirectory", slog.String("dir", dir), slog.Any("error", err))
		return
	}
	found := make(map[string]*files)
	for _, info := range infos {
		name, ok := splitName(info.Name(), shard)
		if !ok || !info.Type().IsRegular() {
			continue
		}
		hashFiles := found[name.hash]
		if hashFiles == nil {
			hashFiles = &files{}
			found[name.hash] = hashFiles
		}
		hashFiles.mark(name.ext)
	}
	for hash, hashFiles := range found {
		c.scanEntry(filepath.Join(dir, hash), hash, *hashFiles)
	}
}

// mark records that a file with extension ext was found. splitName only returns the three
// extensions the cache creates, so any other one is ignored.
func (f *files) mark(ext string) {
	switch ext {
	case dataExt:
		f.data = true
	case metaExt:
		f.meta = true
	case tmpExt:
		f.tmp = true
	default:
	}
}

// fileName is the name of a file the cache could have created, split into its hash and
// its extension.
type fileName struct {
	hash string
	ext  string
}

// splitName splits the name of a file the cache could have created. The boolean is false
// for any other name.
func splitName(name, shard string) (fileName, bool) {
	for _, suffix := range []string{dataExt, metaExt, tmpExt} {
		if stem, found := strings.CutSuffix(name, suffix); found {
			return fileName{hash: stem, ext: suffix}, isLowerHex(stem, hashLen) && strings.HasPrefix(stem, shard)
		}
	}
	return fileName{}, false
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// scanEntry loads one entry or removes its files. A leftover temporary file is always
// removed; a data or metadata file without its partner, or with an unusable one, is too.
func (c *Cache) scanEntry(base, hash string, found files) {
	if found.tmp {
		c.cleanup(base + tmpExt)
	}
	if !found.data || !found.meta {
		c.cleanup(base+dataExt, base+metaExt)
		return
	}
	loaded, err := c.load(base, hash)
	if err != nil {
		c.log.Warn("dropping unusable cache entry", slog.String("name", hash), slog.Any("error", err))
		c.cleanup(base+dataExt, base+metaExt)
		return
	}
	c.entries[loaded.key] = loaded
	c.total += loaded.bytes
}

// cleanup removes files, logging failures: a file that cannot be removed is retried at
// the next Open.
func (c *Cache) cleanup(names ...string) {
	for _, name := range names {
		if err := unlink(c.fs, name); err != nil {
			c.log.Warn("remove stale cache file", slog.Any("error", err))
		}
	}
}

// load reads and checks the metadata file of the entry stored under base.
func (c *Cache) load(base, hash string) (*entry, error) {
	raw, err := c.fs.ReadFile(base + metaExt)
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	var meta metaFile
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parse metadata: %w", err)
	}
	if err := meta.Version.validate(); err != nil {
		return nil, err
	}
	if hashName(meta.Key) != hash {
		return nil, fmt.Errorf("%w: metadata is for %q, not for this file name", ErrInvalidMetadata, meta.Key.Path)
	}
	if err := checkBitmap(meta.Version, meta.Blocks); err != nil {
		return nil, err
	}
	loaded := newEntry(c.opts.Dir, meta.Key, meta.Version, meta.LastRead)
	loaded.bitmap = meta.Blocks
	loaded.validated = meta.Validated
	count := countBlocks(meta.Version, meta.Blocks)
	loaded.present, loaded.bytes = count.present, count.bytes
	return loaded, nil
}
