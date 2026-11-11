package nfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/BlueRival/splicefs/client/core"
)

// dialClient connects to the server. The client library picks its own local port
// without asking the system, so parallel tests can collide; it is asked again.
func dialClient(t *testing.T, s *server) *rpc.Client {
	t.Helper()
	var err error
	for range 100 {
		var client *rpc.Client
		client, err = rpc.DialTCP("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()), false)
		if err == nil {
			return client
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	t.Fatal(err)
	return nil
}

func dial(t *testing.T, s *server) *nfs.Target {
	t.Helper()
	client := dialClient(t, s)
	mount := &nfs.Mount{Client: client}
	target, err := mount.Mount("/example/media", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Close)
	return target
}

func startTest(t *testing.T, fsys *fakeFS) *server {
	t.Helper()
	ln, err := listenLoopback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := startServer(fsys, ln, serverConfig{owner: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// pseudoRandom returns n bytes that do not repeat, so a read at the wrong offset or a
// block served twice is noticed.
func pseudoRandom(n int) []byte {
	rng := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], rng.Uint64())
	}
	return b
}

// wantStatus checks that err is the NFS status with the given number.
func wantStatus(t *testing.T, what string, err error, want uint32) {
	t.Helper()
	var status *nfs.Error
	if !errors.As(err, &status) || status.ErrorNum != want {
		t.Errorf("%s: error = %v, want NFS status %d", what, err, want)
	}
}

func TestProtocolAttributes(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("dir/hello.txt", []byte("hello world"))
	tg := dial(t, startTest(t, fsys))
	file, _ := fsys.node("dir/hello.txt")
	dir, _ := fsys.node("dir")
	mtime := nfs.NFS3Time{Seconds: uint32(fakeMTime.Unix()), Nseconds: uint32(fakeMTime.Nanosecond())}

	tests := []struct {
		name     string
		path     string
		fileType uint32
		perm     uint32
		size     uint64
		inode    uint64
	}{
		{"file", "/dir/hello.txt", nfs.NF3Reg, 0o444, 11, file.attr.Inode},
		{"directory", "/dir", nfs.NF3Dir, 0o555, 0, dir.attr.Inode},
		{"root", "/", nfs.NF3Dir, 0o555, 0, 1},
	}
	for _, tt := range tests {
		got, err := tg.Getattr(tt.path)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		want := nfs.Fattr{
			Type: tt.fileType, FileMode: got.FileMode, Nlink: 1, UID: 501, GID: 20,
			Filesize: tt.size, Used: tt.size, Fileid: tt.inode,
			Atime: mtime, Mtime: mtime, Ctime: mtime,
		}
		if *got != want {
			t.Errorf("%s: attributes = %+v, want %+v", tt.name, *got, want)
		}
		// go-nfs also sets the Go directory bit in the mode; the kernel ignores it.
		if got.FileMode&0o7777 != tt.perm {
			t.Errorf("%s: mode = %o, want %o", tt.name, got.FileMode&0o7777, tt.perm)
		}
	}
}

func TestProtocolReadDirPlusPagesThroughALargeDirectory(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS()
	var want []string
	for i := range 200 {
		name := fmt.Sprintf("file-%03d.txt", i)
		fsys.addFile("many/"+name, make([]byte, i))
		want = append(want, name)
	}
	tg := dial(t, startTest(t, fsys))

	entries, err := tg.ReadDirPlus("/many")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
		node, _ := fsys.node("many/" + e.Name())
		if uint64(e.Size()) != uint64(node.attr.Size) || e.FileId != node.attr.Inode || e.Mode() != 0o444 {
			t.Errorf("%s: size %d id %d mode %v", e.Name(), e.Size(), e.FileId, e.Mode())
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("listed %d names, want %d (first differences: %v)", len(got), len(want), got[:min(3, len(got))])
	}
	// The client asks for about 8 entries per request, so this took 25 requests. The
	// directory must have been read from the core.FS once, not once per request.
	if listed := fsys.listings(); !slices.Equal(listed, []string{"many"}) {
		t.Errorf("core.FS listed %q, want the directory once", listed)
	}
}

func TestProtocolReadsFiles(t *testing.T) {
	t.Parallel()
	data := pseudoRandom(5 << 20)
	tg := dial(t, startTest(t, newFakeFS().addFile("media/big.bin", data)))
	f, err := tg.Open("/media/big.bin")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("whole file in one request", func(t *testing.T) {
		got := make([]byte, len(data))
		if n, err := f.ReadAt(got, 0); n != len(data) || !errors.Is(err, io.EOF) || !bytes.Equal(got, data) {
			t.Errorf("read %d bytes, error %v, equal %v", n, err, bytes.Equal(got, data))
		}
	})
	t.Run("in 64 KiB pieces, out of order", func(t *testing.T) {
		const piece = 64 << 10
		for _, i := range rand.New(rand.NewPCG(3, 4)).Perm(len(data) / piece) {
			buf := make([]byte, piece)
			if n, err := f.ReadAt(buf, int64(i*piece)); n != piece || (err != nil && !errors.Is(err, io.EOF)) {
				t.Fatalf("piece %d: %d bytes, %v", i, n, err)
			}
			if !bytes.Equal(buf, data[i*piece:(i+1)*piece]) {
				t.Fatalf("piece %d differs", i)
			}
		}
	})
	t.Run("sequentially with Read", func(t *testing.T) {
		got, err := io.ReadAll(io.LimitReader(f, int64(len(data))))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("read %d bytes, error %v", len(got), err)
		}
	})
	t.Run("past the end", func(t *testing.T) {
		if n, err := f.ReadAt(make([]byte, 10), int64(len(data))+5); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("read %d bytes, error %v; want 0, EOF", n, err)
		}
	})
}

func TestProtocolReadOnly(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("dir/hello.txt", []byte("hello world"))
	tg := dial(t, startTest(t, fsys))
	f, err := tg.Open("/dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	mode := nfs.SetMode{SetIt: true, Mode: 0o777}

	tests := []struct {
		name string
		call func() error
	}{
		{"create", func() error { _, err := tg.Create("/dir/new.txt", 0o644); return err }},
		{"create over an existing file", func() error { _, err := tg.Create("/dir/hello.txt", 0o644); return err }},
		{"mkdir", func() error { _, err := tg.Mkdir("/dir/sub", 0o755); return err }},
		{"remove", func() error { return tg.Remove("/dir/hello.txt") }},
		{"rmdir", func() error { return tg.RmDir("/dir") }},
		{"rename", func() error { return tg.Rename("/dir/hello.txt", "/dir/moved.txt") }},
		{"symlink", func() error { return tg.Symlink("/dir/hello.txt", "/dir/link") }},
		{"setattr", func() error { return tg.Setattr("/dir/hello.txt", nfs.Sattr3{Mode: mode}) }},
		{"write", func() error { _, err := f.Write([]byte("x")); return err }},
	}
	for _, tt := range tests {
		wantStatus(t, tt.name, tt.call(), nfs.NFS3ErrROFS)
	}
	for _, name := range []string{"dir/new.txt", "dir/sub", "dir/moved.txt", "dir/link"} {
		if _, ok := fsys.node(name); ok {
			t.Errorf("%s was created", name)
		}
	}
	if _, ok := fsys.node("dir/hello.txt"); !ok {
		t.Error("hello.txt was removed")
	}
}

func TestProtocolAccessDeniesEveryWrite(t *testing.T) {
	t.Parallel()
	tg := dial(t, startTest(t, newFakeFS().addFile("hello.txt", []byte("x"))))
	const writes = nfs.ACCESS3_MODIFY | nfs.ACCESS3_EXTEND | nfs.ACCESS3_DELETE
	got, err := tg.Access("/hello.txt", nfs.ACCESS3_READ|writes)
	if err != nil {
		t.Fatal(err)
	}
	if got&nfs.ACCESS3_READ == 0 || got&writes != 0 {
		t.Errorf("access mask = %#x, want read without write", got)
	}
}

func TestProtocolErrors(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("dir/hello.txt", []byte("hello world"))
	tg := dial(t, startTest(t, fsys))

	if _, _, err := tg.Lookup("/dir/missing"); !os.IsNotExist(err) {
		t.Errorf("lookup of a missing name = %v, want not-exist", err)
	}
	_, _, err := tg.Lookup("/dir/hello.txt/below")
	wantStatus(t, "lookup below a file", err, nfs.NFS3ErrNotDir)

	_, fh, err := tg.Lookup("/dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	for name, handle := range map[string][]byte{
		"unknown handle":   bytes.Repeat([]byte{7}, len(fh)),
		"malformed handle": {1, 2, 3},
	} {
		_, err := tg.GetAttr(handle)
		wantStatus(t, name, err, nfs.NFS3ErrStale)
	}

	fsys.mu.Lock()
	delete(fsys.nodes, "dir/hello.txt")
	fsys.mu.Unlock()
	if _, err := tg.GetAttr(fh); !os.IsNotExist(err) {
		t.Errorf("getattr of a removed file = %v, want not-exist", err)
	}
}

// TestProtocolUnavailableIsAnIOError checks what a program sees when the nodes cannot
// be reached: never "not found" where go-nfs lets the layer decide.
func TestProtocolUnavailableIsAnIOError(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("dir/hello.txt", []byte("hello world"))
	tg := dial(t, startTest(t, fsys))
	_, fh, err := tg.Lookup("/dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	file, err := tg.Open("/dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}

	setFailing := func(ops ...string) {
		fsys.mu.Lock()
		defer fsys.mu.Unlock()
		fsys.fail = func(op, _ string) error {
			if slices.Contains(ops, op) {
				return core.ErrUnavailable
			}
			return nil
		}
	}

	setFailing("stat")
	_, err = tg.GetAttr(fh)
	wantStatus(t, "getattr", err, nfs.NFS3ErrIO)

	setFailing("read")
	_, err = file.ReadAt(make([]byte, 5), 0)
	wantStatus(t, "read", err, nfs.NFS3ErrIO)
}

// TestProtocolLimitsOfGoNFS records where go-nfs v0.0.4 answers with a fixed status
// that does not say "I/O error", whatever went wrong: a failed LOOKUP is NFS3ERR_NOENT
// and a failed READDIR is NFS3ERR_NOTDIR. The layer returns the right error to go-nfs;
// go-nfs discards it. If a newer go-nfs fixes this, these expectations should change to
// NFS3ERR_IO.
func TestProtocolLimitsOfGoNFS(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("dir/hello.txt", []byte("hello world"))
	tg := dial(t, startTest(t, fsys))
	if _, _, err := tg.Lookup("/dir"); err != nil {
		t.Fatal(err)
	}

	fsys.mu.Lock()
	fsys.fail = func(op, name string) error {
		if (op == "stat" && name == "dir/hello.txt") || op == "readdir" {
			return core.ErrUnavailable
		}
		return nil
	}
	fsys.mu.Unlock()

	if _, _, err := tg.Lookup("/dir/hello.txt"); !os.IsNotExist(err) {
		t.Errorf("lookup during an outage = %v, want NOENT (go-nfs limit)", err)
	}
	_, err := tg.ReadDirPlus("/dir")
	wantStatus(t, "readdir during an outage", err, nfs.NFS3ErrNotDir)
}

func TestProtocolFindsNamesInAnotherUnicodeForm(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile("m\u00fasica/"+nfcCafe, []byte("content"))
	tg := dial(t, startTest(t, fsys))
	asked := "/mu\u0301sica/" + nfdCafe

	f, err := tg.Open(asked)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 16)
	if n, err := f.ReadAt(got, 0); string(got[:n]) != "content" || (err != nil && !errors.Is(err, io.EOF)) {
		t.Errorf("read %q, %v", got[:n], err)
	}
	entries, err := tg.ReadDirPlus("/mu\u0301sica")
	if err != nil || len(entries) != 1 || entries[0].Name() != nfcCafe {
		t.Errorf("listing = %v, %v; want the stored name %q", entries, err, nfcCafe)
	}
}

func TestProtocolSeveralClientsAtOnce(t *testing.T) {
	t.Parallel()
	data := pseudoRandom(2 << 20)
	s := startTest(t, newFakeFS().addFile("big.bin", data))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		tg := dial(t, s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := tg.Open("/big.bin")
			if err != nil {
				errs <- err
				return
			}
			got := make([]byte, len(data))
			if _, err := f.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
				errs <- err
			} else if !bytes.Equal(got, data) {
				errs <- errors.New("data differs")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
