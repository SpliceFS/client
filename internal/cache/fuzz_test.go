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
	f.Fuzz(func(t *testing.T, meta, body []byte) {
		// The file name comes from the key in the metadata, because only then can the
		// metadata pass the check that it belongs to its file name.
		var m metaFile
		if json.Unmarshal(meta, &m) != nil {
			m.Key = testKey("a")
		}
		dir := filepath.Join(t.TempDir(), "cache")
		base := baseName(dir, m.Key)
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			t.Fatal(err)
		}
		for ext, content := range map[string][]byte{metaExt: meta, dataExt: body} {
			if err := os.WriteFile(base+ext, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c, err := Open(Options{Dir: dir, MaxSize: 1 << 40, MaxAge: 1 << 60})
		if err != nil {
			// Damage to one entry must never keep the whole cache from opening.
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = c.Close() }()
		info, ok := c.Peek(m.Key)
		if !ok {
			// An entry that was refused must not linger, or it would be refused again
			// at every start.
			for _, ext := range []string{metaExt, dataExt} {
				if _, err := os.Stat(base + ext); err == nil {
					t.Fatalf("rejected entry left %s behind", ext)
				}
			}
			return
		}
		checkLoaded(t, c, m.Key, info)
	})
}

// checkLoaded holds what a trusted entry must satisfy for the reads that follow.
func checkLoaded(t *testing.T, c *Cache, key Key, info Info) {
	t.Helper()
	v := info.Version
	if err := v.validate(); err != nil {
		t.Fatalf("accepted an invalid version %+v: %v", v, err)
	}
	// Bytes counts blocks that are claimed present, so it can never exceed the file.
	if info.Bytes < 0 || info.Bytes > v.Size {
		t.Fatalf("Bytes = %d for a file of %d bytes", info.Bytes, v.Size)
	}
	if info.Complete != (info.Bytes == v.Size) {
		t.Fatalf("Complete = %v with %d of %d bytes", info.Complete, info.Bytes, v.Size)
	}
	if v.Size > 1<<16 {
		return
	}
	h, err := c.Acquire(key, v)
	if err != nil {
		t.Fatalf("Acquire of the loaded version: %v", err)
	}
	defer func() { _ = h.Release() }()
	// A data file shorter than the metadata claims must give an error, not a panic or
	// bytes that were never written.
	buf := make([]byte, v.Size)
	n, err := h.ReadAt(buf, 0)
	if n < 0 || int64(n) > v.Size {
		t.Fatalf("ReadAt returned %d, %v for %d bytes", n, err, v.Size)
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
	f.Fuzz(func(t *testing.T, size, blockSize, block int64) {
		v := Version{Size: size, BlockSize: blockSize}
		if v.validate() != nil {
			// Acquire refuses these, so the arithmetic below is never reached with them.
			return
		}
		n := v.blocks()
		if n < 0 || n > maxBlocks || (size > 0 && n == 0) {
			t.Fatalf("%d bytes in blocks of %d gave %d blocks", size, blockSize, n)
		}
		// The blocks must cover the file exactly, with only the last one short.
		if n > 0 {
			last := v.blockLen(n - 1)
			if last < 1 || last > blockSize || (n-1)*blockSize+last != size {
				t.Fatalf("last block of %d bytes in blocks of %d has length %d", size, blockSize, last)
			}
		}
		if got := v.blockLen(block); (block >= 0 && block < n) != (got >= 1) || got > blockSize {
			t.Fatalf("blockLen(%d) = %d with %d blocks of %d", block, got, n, blockSize)
		}
		if bitmapLen(n) < 0 || bitmapLen(n)*8 < n {
			t.Fatalf("bitmapLen(%d) = %d", n, bitmapLen(n))
		}
	})
}
