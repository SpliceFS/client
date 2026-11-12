package fetch

import (
	"errors"
	"io"
	"testing"
)

// FuzzReadAt reads arbitrary offsets and lengths of a file of arbitrary size and checks
// that the blocks fetched and stitched together are exactly the requested range.
func FuzzReadAt(f *testing.F) {
	type read struct {
		off int64
		n   uint16
	}
	for _, seed := range []struct {
		size, blockSize, readAhead uint16
		useStore                   bool
		reads                      [3]read
	}{
		{10000, 100, 300, false, [3]read{{0, 10}, {9990, 10}, {3, 6}}},
		{1000, 100, 0, true, [3]read{{0, 500}, {500, 500}, {990, 50}}},
		{0, 1, 0, false, [3]read{{0, 1}, {0, 0}, {-1, 1}}},
		{7, 3, 1, false, [3]read{{6, 9}, {7, 1}, {1 << 62, 9}}},
		{4096, 1, 4096, false, [3]read{{1<<63 - 1, 2}, {4095, 1}, {0, 4096}}},
	} {
		f.Add(seed.size, seed.blockSize, seed.readAhead, seed.useStore, seed.reads[0].off,
			seed.reads[0].n, seed.reads[1].off, seed.reads[1].n, seed.reads[2].off, seed.reads[2].n)
	}
	f.Fuzz(func(t *testing.T, size, blockSize, readAhead uint16, useStore bool,
		off1 int64, n1 uint16, off2 int64, n2 uint16, off3 int64, n3 uint16) {
		opts := baseOptions(int64(size))
		opts.BlockSize = int64(blockSize)%512 + 1
		opts.ReadAhead = int64(readAhead)
		var store *fakeStore
		if useStore {
			// The fake store has blocks of 100 bytes.
			opts.BlockSize = 100
			store = newFakeStore(int64(size))
		}
		h := newHarness(t, opts, store, nil, "a")
		h.src["a"].setRespond(always(nil))
		for _, r := range []struct {
			off int64
			n   uint16
		}{{off1, n1}, {off2, n2}, {off3, n3}} {
			buf := make([]byte, int(r.n)%4097)
			got, err := h.r.ReadAt(bg, buf, r.off)
			checkRead(t, int64(size), r.off, len(buf), got, err, buf)
		}
		if store != nil {
			store.check(t, int64(size))
		}
	})
}

// checkRead holds the io.ReaderAt contract for a read, so that a wrong block range
// shows as wrong bytes, a wrong count or a wrong error.
func checkRead(t *testing.T, size, off int64, want, got int, err error, buf []byte) {
	t.Helper()
	if off < 0 {
		// A negative offset is a caller error and must not reach the sources.
		if err == nil || got != 0 {
			t.Fatalf("read at %d returned %d, %v", off, got, err)
		}
		return
	}
	avail := max(size-off, 0)
	wantN := min(int64(want), avail)
	if int64(got) != wantN {
		t.Fatalf("read %d at %d of %d bytes returned %d, %v; want %d", want, off, size, got, err, wantN)
	}
	// At or past the end the answer is EOF even for an empty read, as io.ReaderAt allows.
	wantEOF := off >= size || int64(want) > avail
	if wantEOF != errors.Is(err, io.EOF) || (!wantEOF && err != nil) {
		t.Fatalf("read %d at %d of %d bytes returned %d, %v", want, off, size, got, err)
	}
	if string(buf[:got]) != string(fileBytes(off, int64(got))) {
		t.Fatalf("read %d at %d of %d bytes returned the wrong bytes", want, off, size)
	}
}

// FuzzPieces checks the mapping of a byte range to piece indexes and back, which every
// read and every cache write depends on.
func FuzzPieces(f *testing.F) {
	f.Add(int64(0), int64(10), int64(4), int64(10))
	f.Add(int64(3), int64(4), int64(4), int64(100))
	f.Add(int64(1<<62), int64(1<<62+5), int64(1<<20), int64(1<<63-1))
	f.Add(int64(0), int64(1<<20), int64(1), int64(1<<20))
	f.Fuzz(func(t *testing.T, off, length, unit, size int64) {
		// The caller guarantees these; a read is never empty, negative or past the file.
		if unit <= 0 || off < 0 || length <= 0 || size < 0 || off > size-length {
			return
		}
		end := off + length
		// A range this long would only make the test slow; the arithmetic is the same.
		if (end-1)/unit-off/unit > 1<<12 {
			return
		}
		got := pieces(off, end, unit)
		if len(got) == 0 || got[0]*unit > off || (got[len(got)-1]+1)*unit < end {
			t.Fatalf("pieces(%d, %d, %d) = %v does not cover the range", off, end, unit, got)
		}
		for i, p := range got {
			if p < 0 || (i > 0 && p != got[i-1]+1) {
				t.Fatalf("pieces(%d, %d, %d) = %v is not consecutive and non-negative", off, end, unit, got)
			}
			start, n := span(p, unit, size)
			if start < 0 || n < 1 || n > unit || start+n > size {
				t.Fatalf("span(%d, %d, %d) = %d, %d is outside the file", p, unit, size, start, n)
			}
		}
	})
}
