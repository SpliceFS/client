package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rewriteMeta(t *testing.T, base string, edit func(*metaFile)) {
	t.Helper()
	raw, err := os.ReadFile(base + metaExt)
	if err != nil {
		t.Fatal(err)
	}
	var m metaFile
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(&m)
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+metaExt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDropsUnusableEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		damage func(t *testing.T, base string)
	}{
		{"corrupt metadata", func(t *testing.T, base string) {
			if err := os.WriteFile(base+metaExt, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty metadata", func(t *testing.T, base string) {
			if err := os.WriteFile(base+metaExt, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"invalid version", func(t *testing.T, base string) {
			rewriteMeta(t, base, func(m *metaFile) { m.Version.BlockSize = 0 })
		}},
		{"metadata of another file", func(t *testing.T, base string) {
			rewriteMeta(t, base, func(m *metaFile) { m.Key.Path = "/other" })
		}},
		{"bitmap of the wrong size", func(t *testing.T, base string) {
			rewriteMeta(t, base, func(m *metaFile) { m.Blocks = []byte{1, 1} })
		}},
		{"data file missing", func(t *testing.T, base string) {
			if err := os.Remove(base + dataExt); err != nil {
				t.Fatal(err)
			}
		}},
		{"metadata missing", func(t *testing.T, base string) {
			if err := os.Remove(base + metaExt); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.open()
			key := testKey("a")
			fill(t, c, key, testVersion(8), 0)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			base := baseName(e.opts.Dir, key)
			tt.damage(t, base)

			c2 := e.open()
			if _, ok := c2.Peek(key); ok {
				t.Error("unusable entry was loaded")
			}
			mustNotExist(t, base+dataExt)
			mustNotExist(t, base+metaExt)
			if u := c2.Usage(); u.Entries != 0 || u.Bytes != 0 {
				t.Errorf("Usage = %+v", u)
			}
		})
	}
}

func TestOpenRemovesLeftoverTemporaryFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	kept, alone := testKey("kept"), testKey("alone")
	fill(t, c, kept, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{kept, alone} {
		base := baseName(e.opts.Dir, key)
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+tmpExt, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c2 := e.open()
	if _, ok := c2.Peek(kept); !ok {
		t.Error("entry next to a temporary file was dropped")
	}
	mustNotExist(t, baseName(e.opts.Dir, kept)+tmpExt)
	mustNotExist(t, baseName(e.opts.Dir, alone)+tmpExt)
}

// unusedShards returns n shard directory names that no test key hashes into.
func unusedShards(n int, keys ...Key) []string {
	taken := map[string]bool{}
	for _, k := range keys {
		taken[hashName(k)[:shardLen]] = true
	}
	var out []string
	for i := 0; len(out) < n; i++ {
		if name := fmt.Sprintf("%02x", i); !taken[name] {
			out = append(out, name)
		}
	}
	return out
}

func TestOpenLeavesUnrecognizedFilesAlone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, other := testKey("a"), testKey("other")
	fill(t, c, key, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	hash := hashName(other)
	shard := filepath.Join(e.opts.Dir, hash[:shardLen])
	spare := unusedShards(2, key, other)
	foreignFiles := []string{
		filepath.Join(e.opts.Dir, "notes.txt"),
		filepath.Join(e.opts.Dir, "zz", hash+dataExt), // not a shard directory
		filepath.Join(shard, "readme.txt"),
		filepath.Join(shard, hash+".bak"),
		filepath.Join(shard, strings.ToUpper(hash)+dataExt),
		filepath.Join(shard, hash[:hashLen-4]+dataExt),
		filepath.Join(e.opts.Dir, spare[0], hash+dataExt), // right name, wrong shard
		filepath.Join(e.opts.Dir, spare[1]),               // a file named like a shard
	}
	for _, f := range foreignFiles {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with the name of a metadata file is not a file the cache wrote.
	foreignDir := filepath.Join(shard, hash+metaExt)
	if err := os.Mkdir(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}

	c2 := e.open()
	if _, ok := c2.Peek(key); !ok {
		t.Error("the valid entry was lost")
	}
	for _, f := range append(foreignFiles, foreignDir) {
		mustExist(t, f)
	}
}

func TestOpenSurvivesUnreadableFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	good, bad, orphan := testKey("good"), testKey("bad"), testKey("orphan")
	v := testVersion(8)
	fill(t, c, good, v, 0)
	fill(t, c, bad, v, 0)
	fill(t, c, orphan, v, 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(baseName(e.opts.Dir, orphan) + metaExt); err != nil {
		t.Fatal(err)
	}
	badBase := baseName(e.opts.Dir, bad)
	// The metadata of bad cannot be read, and nothing can be removed.
	fsys := faultFS{fail: func(op, name string) error {
		switch {
		case op == "readfile" && name == badBase+metaExt:
			return errInjected
		case op == "remove":
			return errInjected
		}
		return nil
	}}
	c2 := e.openFS(fsys)
	if _, ok := c2.Peek(good); !ok {
		t.Error("good entry not loaded")
	}
	if _, ok := c2.Peek(bad); ok {
		t.Error("unreadable entry loaded")
	}
	mustContain(t, e.logs, "dropping unusable cache entry")
	mustContain(t, e.logs, "remove stale cache file")
}

func TestOpenSurvivesUnreadableSubdirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	good, bad := testKey("good"), testKey("bad")
	fill(t, c, good, testVersion(8), 0)
	fill(t, c, bad, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Dir(baseName(e.opts.Dir, bad))
	c2 := e.openFS(faultFS{fail: func(op, name string) error {
		if op == "readdir" && name == badDir {
			return errInjected
		}
		return nil
	}})
	if _, ok := c2.Peek(bad); ok {
		t.Error("entry in an unreadable subdirectory was loaded")
	}
	if _, ok := c2.Peek(good); !ok {
		t.Error("an unreadable subdirectory stopped the scan")
	}
	mustContain(t, e.logs, "scan cache subdirectory")
}
