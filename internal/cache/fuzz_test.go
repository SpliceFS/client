package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzOpenMetadata opens a cache directory whose metadata file holds arbitrary bytes,
// as a crash or a failing disk may leave it.
func FuzzOpenMetadata(f *testing.F) {
	good, err := json.Marshal(metaFile{
		Key:     testKey("a"),
		Version: testVersion(10),
		Blocks:  []byte{0b101},
	})
	if err != nil {
		f.Fatal(err)
	}
	data := content(10)
	f.Add(good, data)
	f.Add(good[:len(good)/2], data)
	f.Add(good, data[:3])
	f.Add([]byte(""), []byte(""))
	f.Add([]byte("{not json"), data)
	for _, version := range []string{
		`{"size":-1,"block_size":4}`,
		`{"size":9223372036854775807,"block_size":1}`,
		`{"size":9223372036854775807,"block_size":9223372036854775807},"blocks":"AQ=="`,
		`{"size":10,"block_size":4},"blocks":"/w=="`,
		`{"size":10,"block_size":4},"blocks":null`,
	} {
		f.Add([]byte(`{"key":{"cluster":"c","path":"/p"},"version":`+version+`}`), data)
	}
	f.Fuzz(openMetadataCase)
}

// openMetadataCase writes one entry with the given metadata and data file and opens the
// cache on it.
func openMetadataCase(t *testing.T, meta, body []byte) {
	t.Helper()
	// The file name comes from the key in the metadata, because only then can the
	// metadata pass the check that it belongs to its file name.
	var parsed metaFile
	if json.Unmarshal(meta, &parsed) != nil {
		parsed.Key = testKey("a")
	}
	dir := filepath.Join(t.TempDir(), "cache")
	base := baseName(dir, parsed.Key)
	writeEntryFiles(t, base, meta, body)
	cache, err := Open(Options{Dir: dir, MaxSize: 1 << 40, MaxAge: 1 << 60})
	if err != nil {
		// Damage to one entry must never keep the whole cache from opening.
		t.Fatalf("Open: %v", err)
	}
	defer closeCache(t, cache)
	info, ok := cache.Peek(parsed.Key)
	if !ok {
		checkRejectedGone(t, base)
		return
	}
	checkLoaded(t, cache, parsed.Key, info)
}

// writeEntryFiles puts the metadata file and the data file of one entry on disk.
func writeEntryFiles(t *testing.T, base string, meta, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	for ext, raw := range map[string][]byte{metaExt: meta, dataExt: body} {
		if err := os.WriteFile(base+ext, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// checkRejectedGone fails if a refused entry left a file behind: it would be refused
// again at every start.
func checkRejectedGone(t *testing.T, base string) {
	t.Helper()
	for _, ext := range []string{metaExt, dataExt} {
		if _, err := os.Stat(base + ext); err == nil {
			t.Fatalf("rejected entry left %s behind", ext)
		}
	}
}

// checkLoaded holds what a trusted entry must satisfy for the reads that follow.
func checkLoaded(t *testing.T, cache *Cache, key Key, info Info) {
	t.Helper()
	version := info.Version
	if err := version.validate(); err != nil {
		t.Fatalf("accepted an invalid version %+v: %v", version, err)
	}
	// Bytes counts blocks that are claimed present, so it can never exceed the file.
	if info.Bytes < 0 || info.Bytes > version.Size {
		t.Fatalf("Bytes = %d for a file of %d bytes", info.Bytes, version.Size)
	}
	if info.Complete != (info.Bytes == version.Size) {
		t.Fatalf("Complete = %v with %d of %d bytes", info.Complete, info.Bytes, version.Size)
	}
	if version.Size > 1<<16 {
		return
	}
	handle, err := cache.Acquire(key, version)
	if err != nil {
		t.Fatalf("Acquire of the loaded version: %v", err)
	}
	defer release(t, handle)
	// A data file shorter than the metadata claims must give an error, not a panic or
	// bytes that were never written.
	buf := make([]byte, version.Size)
	n, err := handle.ReadAt(buf, 0)
	if n < 0 || int64(n) > version.Size {
		t.Fatalf("ReadAt returned %d, %v for %d bytes", n, err, version.Size)
	}
}

// FuzzVersionBlocks checks the block arithmetic for sizes that come from a server or a
// damaged metadata file, including ones near the limits of int64.
func FuzzVersionBlocks(f *testing.F) {
	f.Add(int64(10), int64(4), int64(2))
	f.Add(int64(0), int64(1), int64(0))
	f.Add(int64(-1), int64(4), int64(0))
	f.Add(int64(10), int64(0), int64(0))
	f.Add(int64(1<<62), int64(1<<40), int64(3))
	f.Add(int64(1<<63-1), int64(1<<63-1), int64(1))
	f.Add(int64(1<<63-1), int64(1), int64(-1))
	f.Fuzz(checkVersionBlocks)
}

// checkVersionBlocks checks the block arithmetic of one version.
func checkVersionBlocks(t *testing.T, size, blockSize, block int64) {
	t.Helper()
	version := Version{Size: size, BlockSize: blockSize}
	if version.validate() != nil {
		// Acquire refuses these, so the arithmetic below is never reached with them.
		return
	}
	count := version.blocks()
	if count < 0 || count > maxBlocks || (size > 0 && count == 0) {
		t.Fatalf("%d bytes in blocks of %d gave %d blocks", size, blockSize, count)
	}
	checkLastBlock(t, version, count)
	checkBlockLen(t, version, count, block)
	if bitmapLen(count) < 0 || bitmapLen(count)*bitsPerByte < count {
		t.Fatalf("bitmapLen(%d) = %d", count, bitmapLen(count))
	}
}

// checkBlockLen checks that blockLen is positive for the blocks the file has, and only
// for those.
func checkBlockLen(t *testing.T, version Version, count, block int64) {
	t.Helper()
	got := version.blockLen(block)
	if (block >= 0 && block < count) != (got >= 1) || got > version.BlockSize {
		t.Fatalf("blockLen(%d) = %d with %d blocks of %d", block, got, count, version.BlockSize)
	}
}

// checkLastBlock checks that the blocks cover the file exactly, with only the last one
// short.
func checkLastBlock(t *testing.T, version Version, count int64) {
	t.Helper()
	if count == 0 {
		return
	}
	last := version.blockLen(count - 1)
	if last < 1 || last > version.BlockSize || (count-1)*version.BlockSize+last != version.Size {
		t.Fatalf("last block of %d bytes in blocks of %d has length %d", version.Size, version.BlockSize, last)
	}
}
