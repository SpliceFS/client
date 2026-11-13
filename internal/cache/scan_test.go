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
	var meta metaFile
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	edit(&meta)
	if raw, err = json.Marshal(meta); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(base+metaExt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// overwriteMeta damages an entry by replacing its metadata file with raw.
func overwriteMeta(raw []byte) func(*testing.T, string) {
	return func(t *testing.T, base string) {
		t.Helper()
		if err := os.WriteFile(base+metaExt, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// editMeta damages an entry by changing the content of its metadata file.
func editMeta(edit func(*metaFile)) func(*testing.T, string) {
	return func(t *testing.T, base string) {
		t.Helper()
		rewriteMeta(t, base, edit)
	}
}

// removeFile damages an entry by deleting one of its two files.
func removeFile(ext string) func(*testing.T, string) {
	return func(t *testing.T, base string) {
		t.Helper()
		if err := os.Remove(base + ext); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenDropsUnusableEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		damage func(t *testing.T, base string)
	}{
		{"corrupt metadata", overwriteMeta([]byte("{not json"))},
		{"empty metadata", overwriteMeta(nil)},
		{"invalid version", editMeta(func(meta *metaFile) { meta.Version.BlockSize = 0 })},
		{"metadata of another file", editMeta(func(meta *metaFile) { meta.Key.Path = "/other" })},
		{"bitmap of the wrong size", editMeta(func(meta *metaFile) { meta.Blocks = []byte{1, 1} })},
		{"data file missing", removeFile(dataExt)},
		{"metadata missing", removeFile(metaExt)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newEnv(t)
			c := env.open()
			key := testKey("a")
			fill(t, c, key, testVersion(8), 0)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			base := baseName(env.opts.Dir, key)
			tt.damage(t, base)

			reopened := env.open()
			if _, ok := reopened.Peek(key); ok {
				t.Error("unusable entry was loaded")
			}
			mustNotExist(t, base+dataExt)
			mustNotExist(t, base+metaExt)
			if u := reopened.Usage(); u.Entries != 0 || u.Bytes != 0 {
				t.Errorf("Usage = %+v", u)
			}
		})
	}
}

func TestOpenRemovesLeftoverTemporaryFiles(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	c := env.open()
	kept, alone := testKey("kept"), testKey("alone")
	fill(t, c, kept, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{kept, alone} {
		base := baseName(env.opts.Dir, key)
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(base+tmpExt, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	c2 := env.open()
	if _, ok := c2.Peek(kept); !ok {
		t.Error("entry next to a temporary file was dropped")
	}
	mustNotExist(t, baseName(env.opts.Dir, kept)+tmpExt)
	mustNotExist(t, baseName(env.opts.Dir, alone)+tmpExt)
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
	env := newEnv(t)
	c := env.open()
	key, other := testKey("a"), testKey("other")
	fill(t, c, key, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	hash := hashName(other)
	shard := filepath.Join(env.opts.Dir, hash[:shardLen])
	spare := unusedShards(2, key, other)
	foreignFiles := []string{
		filepath.Join(env.opts.Dir, "notes.txt"),
		filepath.Join(env.opts.Dir, "zz", hash+dataExt), // not a shard directory
		filepath.Join(shard, "readme.txt"),
		filepath.Join(shard, hash+".bak"),
		filepath.Join(shard, strings.ToUpper(hash)+dataExt),
		filepath.Join(shard, hash[:hashLen-4]+dataExt),
		filepath.Join(env.opts.Dir, spare[0], hash+dataExt), // right name, wrong shard
		filepath.Join(env.opts.Dir, spare[1]),               // a file named like a shard
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

	c2 := env.open()
	if _, ok := c2.Peek(key); !ok {
		t.Error("the valid entry was lost")
	}
	for _, f := range append(foreignFiles, foreignDir) {
		mustExist(t, f)
	}
}

func TestOpenSurvivesUnreadableFiles(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	good, bad, orphan := testKey("good"), testKey("bad"), testKey("orphan")
	v := testVersion(8)
	fill(t, cache, good, v, 0)
	fill(t, cache, bad, v, 0)
	fill(t, cache, orphan, v, 0)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(baseName(env.opts.Dir, orphan) + metaExt); err != nil {
		t.Fatal(err)
	}
	badBase := baseName(env.opts.Dir, bad)
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
	c2 := env.openFS(fsys)
	if _, ok := c2.Peek(good); !ok {
		t.Error("good entry not loaded")
	}
	if _, ok := c2.Peek(bad); ok {
		t.Error("unreadable entry loaded")
	}
	mustContain(t, env.logs, "dropping unusable cache entry")
	mustContain(t, env.logs, "remove stale cache file")
}

func TestOpenSurvivesUnreadableSubdirectory(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	c := env.open()
	good, bad := testKey("good"), testKey("bad")
	fill(t, c, good, testVersion(8), 0)
	fill(t, c, bad, testVersion(8), 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Dir(baseName(env.opts.Dir, bad))
	reopened := env.openFS(faultFS{fail: func(op, name string) error {
		if op == "readdir" && name == badDir {
			return errInjected
		}
		return nil
	}})
	if _, ok := reopened.Peek(bad); ok {
		t.Error("entry in an unreadable subdirectory was loaded")
	}
	if _, ok := reopened.Peek(good); !ok {
		t.Error("an unreadable subdirectory stopped the scan")
	}
	mustContain(t, env.logs, "scan cache subdirectory")
}
