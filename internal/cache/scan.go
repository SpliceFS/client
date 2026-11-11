package cache

import (
	"encoding/json"
	"fmt"
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
		c.log.Warn("scan cache subdirectory", "dir", dir, "error", err)
		return
	}
	found := make(map[string]*files)
	for _, info := range infos {
		hash, ext, ok := splitName(info.Name(), shard)
		if !ok || !info.Type().IsRegular() {
			continue
		}
		f := found[hash]
		if f == nil {
			f = &files{}
			found[hash] = f
		}
		switch ext {
		case dataExt:
			f.data = true
		case metaExt:
			f.meta = true
		case tmpExt:
			f.tmp = true
		}
	}
	for hash, f := range found {
		c.scanEntry(filepath.Join(dir, hash), hash, *f)
	}
}

// splitName splits the name of a file the cache could have created into its hash and
// extension. ok is false for any other name.
func splitName(name, shard string) (hash, ext string, ok bool) {
	for _, suffix := range []string{dataExt, metaExt, tmpExt} {
		if stem, found := strings.CutSuffix(name, suffix); found {
			return stem, suffix, isLowerHex(stem, hashLen) && strings.HasPrefix(stem, shard)
		}
	}
	return "", "", false
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
func (c *Cache) scanEntry(base, hash string, f files) {
	if f.tmp {
		c.cleanup(base + tmpExt)
	}
	if !f.data || !f.meta {
		c.cleanup(base+dataExt, base+metaExt)
		return
	}
	e, err := c.load(base, hash)
	if err != nil {
		c.log.Warn("dropping unusable cache entry", "name", hash, "error", err)
		c.cleanup(base+dataExt, base+metaExt)
		return
	}
	c.entries[e.key] = e
	c.total += e.bytes
}

// cleanup removes files, logging failures: a file that cannot be removed is retried at
// the next Open.
func (c *Cache) cleanup(names ...string) {
	for _, name := range names {
		c.warn("remove stale cache file", unlink(c.fs, name))
	}
}

// load reads and checks the metadata file of the entry stored under base.
func (c *Cache) load(base, hash string) (*entry, error) {
	raw, err := c.fs.ReadFile(base + metaExt)
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	var m metaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse metadata: %w", err)
	}
	if err := m.Version.validate(); err != nil {
		return nil, err
	}
	if hashName(m.Key) != hash {
		return nil, fmt.Errorf("metadata is for %q, not for this file name", m.Key.Path)
	}
	if err := checkBitmap(m.Version, m.Blocks); err != nil {
		return nil, err
	}
	e := newEntry(c.opts.Dir, m.Key, m.Version, m.LastRead)
	e.bitmap = m.Blocks
	e.validated = m.Validated
	e.present, e.bytes = countBlocks(m.Version, m.Blocks)
	return e, nil
}
