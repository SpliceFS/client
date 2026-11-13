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
func mountOrSkip(t *testing.T, env environment, fsys *fakeFS) *mounted {
	t.Helper()
	mnt, err := mountWith(fsys, mount.Options{Target: t.TempDir(), Source: "splicefs:accept/"}, env)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied") {
			t.Skipf("mount_nfs cannot be run by this user without root: %v", err)
		}
		t.Fatal(err)
	}
	live := asMounted(t, mnt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if unmountErr := live.Unmount(ctx, true); unmountErr != nil {
			t.Errorf("forced unmount at the end of the test: %v", unmountErr)
		}
		<-live.Done()
	})
	return live
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

func TestRealMountListsAndReads(t *testing.T) {
	t.Parallel()
	dir := mountOrSkip(t, systemEnvironment(), realTree()).target

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"empty", "music", "video"}; !slices.Equal(names, want) {
		t.Errorf("entries = %q, want %q", names, want)
	}
	want := pseudoRandom(3 << 20)
	got, err := os.ReadFile(filepath.Join(dir, "video", "big.bin"))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("read %d bytes, error %v, equal %v", len(got), err, bytes.Equal(got, want))
	}
}

func TestRealMountAnswersTheAttributesOfAFile(t *testing.T) {
	t.Parallel()
	fsys := realTree()
	dir := mountOrSkip(t, systemEnvironment(), fsys).target

	info, err := os.Stat(filepath.Join(dir, "video", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fsys.node("video/big.bin")
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() is a %T, want *syscall.Stat_t", info.Sys())
	}
	if info.Mode() != 0o444 || info.Size() != 3<<20 || info.ModTime().Unix() != fakeMTime().Unix() {
		t.Errorf("mode %v size %d mtime %v", info.Mode(), info.Size(), info.ModTime())
	}
	if stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || stat.Ino != node.attr.Inode {
		t.Errorf("nlink %d uid %d ino %d (want uid %d ino %d)",
			stat.Nlink, stat.Uid, stat.Ino, os.Getuid(), node.attr.Inode)
	}
}

func TestRealMountAnswersTheAttributesOfADirectory(t *testing.T) {
	t.Parallel()
	dir := mountOrSkip(t, systemEnvironment(), realTree()).target

	info, err := os.Stat(filepath.Join(dir, "music"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o555 {
		t.Errorf("directory: %v, %v", info, err)
	}
}

func TestRealMountRefusesWrites(t *testing.T) {
	t.Parallel()
	dir := mountOrSkip(t, systemEnvironment(), realTree()).target

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
}

func TestRealMountFindsAccentedNamesInEitherForm(t *testing.T) {
	t.Parallel()
	dir := mountOrSkip(t, systemEnvironment(), realTree()).target

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
	// The name is a constant: the linter reads the escape in a literal as a path separator.
	const missing = "missing\u00e9.txt"
	if _, err := os.Stat(filepath.Join(dir, "music", missing)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing accented name = %v, want not-exist", err)
	}
}

func TestRealMountReportsAnUnavailableClusterAsAnIOError(t *testing.T) {
	t.Parallel()
	fsys := realTree()
	dir := mountOrSkip(t, systemEnvironment(), fsys).target

	fsys.mu.Lock()
	fsys.fail = func(op, _ string) error {
		if op == "read" {
			return core.ErrUnavailable
		}
		return nil
	}
	fsys.mu.Unlock()
	// A file nothing has read yet, so the kernel has no data of it to serve.
	_, err := os.ReadFile(filepath.Join(dir, "video", "never-read.bin"))
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("read during an outage = %v, want EIO", err)
	}
}

func TestRealMountUnmounts(t *testing.T) {
	t.Parallel()
	live := mountOrSkip(t, systemEnvironment(), realTree())
	dir := live.target

	if err := live.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	<-live.Done()
	if live.Err() != nil {
		t.Errorf("Err() = %v", live.Err())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the target still shows the mount: %v, %v", entries, err)
	}
}

func TestRealMountEndsWhenUnmountedFromOutside(t *testing.T) {
	t.Parallel()
	env := systemEnvironment()
	// Polling the mount table every few milliseconds saves the test from waiting.
	env.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		ticker := time.NewTicker(5 * time.Millisecond)
		return ticker.C, ticker.Stop
	}
	live := mountOrSkip(t, env, realTree())

	out, err := exec.CommandContext(t.Context(), "/sbin/umount", live.target).CombinedOutput()
	if err != nil {
		t.Fatalf("umount: %v: %s", err, out)
	}
	<-live.Done()
	if live.Err() != nil {
		t.Errorf("Err() = %v after an unmount from outside", live.Err())
	}
}

func TestRealMountIsClearedWhenTheServerDies(t *testing.T) {
	t.Parallel()
	live := mountOrSkip(t, systemEnvironment(), realTree())
	dir := live.target
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	}

	breakServer(t, live)
	<-live.Done()

	if err := live.Err(); err == nil || !strings.Contains(err.Error(), "serve nfs") {
		t.Errorf("Err() = %v, want the server failure", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the dead mount was left behind: %v, %v", entries, err)
	}
}

func TestRealMountRefusesToUnmountWhileInUse(t *testing.T) {
	t.Parallel()
	live := mountOrSkip(t, systemEnvironment(), realTree())
	dir := live.target
	open, err := os.Open(filepath.Join(dir, "video", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, open)

	if err := live.Unmount(t.Context(), false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Unmount of a mount with an open file = %v, want EBUSY", err)
	}
	if err := live.Unmount(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	<-live.Done()
}

// TestRealMountIsFoundInTheMountTable is the regression test for a source with a
// trailing slash: the kernel lists it without, and the mount was then never found.
func TestRealMountIsFoundInTheMountTable(t *testing.T) {
	t.Parallel()
	env := systemEnvironment()
	live := mountOrSkip(t, env, realTree())
	if found, err := env.isMounted(live.target, live.from); err != nil || !found {
		t.Errorf("isMounted(%q, %q) = %v, %v", live.target, live.from, found, err)
	}
}
