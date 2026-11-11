//go:build darwin

package nfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// mountOrSkip mounts fsys on a new temporary directory with the real mount_nfs. The
// mount is forced off when the test ends, whatever happens. When the machine does not
// let this user mount, the test is skipped, never run with sudo.
func mountOrSkip(t *testing.T, env environment, fsys *fakeFS) (*mounted, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := mountWith(fsys, mount.Options{Target: dir, Source: "splicefs:accept/"}, env)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied") {
			t.Skipf("mount_nfs cannot be run by this user without root: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = m.Unmount(ctx, true)
		<-m.Done()
	})
	return m.(*mounted), dir
}

func realTree() *fakeFS {
	return newFakeFS().
		addFile("music/"+nfcCafe, []byte("stored in NFC")).
		addFile("music/r\u00e9sum\u00e9.txt", []byte("stored in NFC, too")).
		addFile("music/re\u0301sume\u0301-nfd.txt", []byte("stored in NFD")).
		addFile("video/big.bin", pseudoRandom(3<<20)).
		addFile("video/never-read.bin", []byte("not in any kernel cache")).
		addDir("empty")
}

func TestRealMount(t *testing.T) {
	fsys := realTree()
	m, dir := mountOrSkip(t, systemEnvironment(), fsys)

	t.Run("lists and reads", func(t *testing.T) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if want := []string{"empty", "music", "video"}; !slices.Equal(names, want) {
			t.Errorf("entries = %q, want %q", names, want)
		}
		want := pseudoRandom(3 << 20)
		got, err := os.ReadFile(filepath.Join(dir, "video", "big.bin"))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("read %d bytes, error %v, equal %v", len(got), err, bytes.Equal(got, want))
		}
	})

	t.Run("attributes", func(t *testing.T) {
		info, err := os.Stat(filepath.Join(dir, "video", "big.bin"))
		if err != nil {
			t.Fatal(err)
		}
		node, _ := fsys.node("video/big.bin")
		st := info.Sys().(*syscall.Stat_t)
		if info.Mode() != 0o444 || info.Size() != 3<<20 || info.ModTime().Unix() != fakeMTime.Unix() ||
			st.Nlink != 1 || int(st.Uid) != os.Getuid() || st.Ino != node.attr.Inode {
			t.Errorf("mode %v size %d mtime %v nlink %d uid %d ino %d (want uid %d ino %d)",
				info.Mode(), info.Size(), info.ModTime(), st.Nlink, st.Uid, st.Ino, os.Getuid(), node.attr.Inode)
		}
		info, err = os.Stat(filepath.Join(dir, "music"))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o555 {
			t.Errorf("directory: %v, %v", info, err)
		}
	})

	t.Run("refuses writes", func(t *testing.T) {
		target := filepath.Join(dir, "video", "big.bin")
		tests := map[string]error{
			"create":    os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644),
			"overwrite": os.WriteFile(target, []byte("x"), 0o644),
			"mkdir":     os.Mkdir(filepath.Join(dir, "sub"), 0o755),
			"remove":    os.Remove(target),
			"rename":    os.Rename(target, filepath.Join(dir, "moved.bin")),
			"chmod":     os.Chmod(target, 0o644),
			"symlink":   os.Symlink("x", filepath.Join(dir, "link")),
		}
		for op, err := range tests {
			if !errors.Is(err, syscall.EROFS) {
				t.Errorf("%s = %v, want a read-only file system error", op, err)
			}
		}
	})

	t.Run("finds accented names in either form", func(t *testing.T) {
		tests := []struct {
			asked string
			want  string
		}{
			{"music/" + nfcCafe, "stored in NFC"},
			{"music/" + nfdCafe, "stored in NFC"},
			{"music/r\u00e9sum\u00e9.txt", "stored in NFC, too"},
			{"music/re\u0301sume\u0301.txt", "stored in NFC, too"},
			{"music/re\u0301sume\u0301-nfd.txt", "stored in NFD"},
			{"music/r\u00e9sum\u00e9-nfd.txt", "stored in NFD"},
		}
		for _, tt := range tests {
			got, err := os.ReadFile(filepath.Join(dir, tt.asked))
			if err != nil || string(got) != tt.want {
				t.Errorf("%q: %q, %v; want %q", tt.asked, got, err, tt.want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "music", "missing\u00e9.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing accented name = %v, want not-exist", err)
		}
	})

	t.Run("an unavailable cluster is an I/O error", func(t *testing.T) {
		fsys.mu.Lock()
		fsys.fail = func(op, _ string) error {
			if op == "read" {
				return core.ErrUnavailable
			}
			return nil
		}
		fsys.mu.Unlock()
		defer func() {
			fsys.mu.Lock()
			fsys.fail = nil
			fsys.mu.Unlock()
		}()
		// A file nothing has read yet, so the kernel has no data of it to serve.
		_, err := os.ReadFile(filepath.Join(dir, "video", "never-read.bin"))
		if !errors.Is(err, syscall.EIO) {
			t.Errorf("read during an outage = %v, want EIO", err)
		}
	})

	t.Run("unmounts", func(t *testing.T) {
		if err := m.Unmount(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		<-m.Done()
		if m.Err() != nil {
			t.Errorf("Err() = %v", m.Err())
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("the target still shows the mount: %v, %v", entries, err)
		}
	})
}

func TestRealMountEndsWhenUnmountedFromOutside(t *testing.T) {
	env := systemEnvironment()
	env.newTicker = fastTicker
	m, dir := mountOrSkip(t, env, realTree())
	resolved, _ := filepath.EvalSymlinks(dir)

	if out, err := exec.CommandContext(t.Context(), "/sbin/umount", resolved).CombinedOutput(); err != nil {
		t.Fatalf("umount: %v: %s", err, out)
	}
	<-m.Done()
	if m.Err() != nil {
		t.Errorf("Err() = %v after an unmount from outside", m.Err())
	}
}

func TestRealMountIsClearedWhenTheServerDies(t *testing.T) {
	m, dir := mountOrSkip(t, systemEnvironment(), realTree())
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	}

	_ = m.srv.guard.Listener.Close()
	<-m.Done()

	if err := m.Err(); err == nil || !strings.Contains(err.Error(), "serve nfs") {
		t.Errorf("Err() = %v, want the server failure", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the dead mount was left behind: %v, %v", entries, err)
	}
}

func TestRealMountRefusesToUnmountWhileInUse(t *testing.T) {
	m, dir := mountOrSkip(t, systemEnvironment(), realTree())
	open, err := os.Open(filepath.Join(dir, "video", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, open)

	if err := m.Unmount(t.Context(), false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Unmount of a mount with an open file = %v, want EBUSY", err)
	}
	if err := m.Unmount(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
}

// fastTicker polls the mount table every few milliseconds so tests need not wait.
func fastTicker(time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(5 * time.Millisecond)
	return t.C, t.Stop
}

// TestRealMountIsFoundInTheMountTable is the regression test for a source with a
// trailing slash: the kernel lists it without, and the mount was then never found.
func TestRealMountIsFoundInTheMountTable(t *testing.T) {
	env := systemEnvironment()
	m, _ := mountOrSkip(t, env, realTree())
	if found, err := env.isMounted(m.target, m.from); err != nil || !found {
		t.Errorf("isMounted(%q, %q) = %v, %v", m.target, m.from, found, err)
	}
}
