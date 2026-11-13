//go:build linux

//nolint:paralleltest // the tests mount for real and share the mount table and the process environment
package fuse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/mount"
)

// These tests hold what only the Linux kernel and its tools can show: the mount table in
// /proc, extended attributes, the fusermount3 helper, lazy detach and the connection
// control directory. The shared tests are in integration_test.go.

const crashEnv = "SPLICEFS_FUSE_TEST_CRASH_TARGET"

// TestMain doubles as the "client" that the dead mount test crashes: when the
// environment names a target, the test binary mounts a fake there and waits to be killed.
func TestMain(m *testing.M) {
	if target := os.Getenv(crashEnv); target != "" {
		os.Exit(runCrashingClient(target))
	}
	m.Run()
}

func runCrashingClient(target string) int {
	mounted, err := Mount(newFakeFS(), mount.Options{Target: target, Source: testSource})
	if err != nil {
		log.New(os.Stderr, "", 0).Println(err)
		return 2
	}
	log.New(os.Stdout, "", 0).Println("ready")
	<-mounted.Done()
	return 0
}

type mountEntry struct {
	fstype, source string
	mountOptions   []string
	superOptions   []string
}

// findMount looks the target up in the mount table of this process.
func findMount(t *testing.T, target string) (mountEntry, bool) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(left)
		rest := strings.Fields(right)
		if len(fields) < 6 || len(rest) < 3 || fields[4] != resolved {
			continue
		}
		return mountEntry{
			fstype:       rest[0],
			source:       rest[1],
			mountOptions: strings.Split(fields[5], ","),
			superOptions: strings.Split(rest[2], ","),
		}, true
	}
	return mountEntry{}, false
}

func TestMountIsDescribedInTheMountTable(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	entry, ok := findMount(t, target)
	if !ok {
		t.Fatal("mount not found in /proc/self/mountinfo")
	}
	if entry.fstype != "fuse.splicefs" || entry.source != testSource {
		t.Fatalf("mount is %q from %q, want fuse.splicefs from %q", entry.fstype, entry.source, testSource)
	}
	if !slices.Contains(entry.mountOptions, "ro") || !slices.Contains(entry.superOptions, "ro") {
		t.Fatalf("mount options %v and super options %v must both be ro", entry.mountOptions, entry.superOptions)
	}
}

// mayAllowOther reports whether this user may mount with allow_other: root always, other
// users only if the administrator enabled it for fusermount3.
func mayAllowOther() bool {
	if os.Geteuid() == 0 {
		return true
	}
	conf, err := os.ReadFile("/etc/fuse.conf")
	if err != nil {
		return false
	}
	return slices.Contains(strings.Fields(string(conf)), "user_allow_other")
}

func TestAllowOtherIsPassedToTheKernel(t *testing.T) {
	skipWithoutFuse(t)
	if !mayAllowOther() {
		t.Skip("allow_other for ordinary users needs user_allow_other in /etc/fuse.conf")
	}
	target := t.TempDir()
	opts := mount.Options{Target: target, Source: testSource, AllowOther: true}
	mnt, err := mountWith(newFakeFS(), opts, defaultSettings(), newKernelHost())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { endMount(t, mnt) })
	entry, ok := findMount(t, target)
	if !ok || !slices.Contains(entry.superOptions, "allow_other") {
		t.Fatalf("super options %v lack allow_other", entry.superOptions)
	}
}

// GNU dd is what offers iflag=direct, and the Linux kernel is what makes a direct read
// interruptible, which is why this test is not shared.
func TestInterruptedReadCancelsTheCoreCall(t *testing.T) {
	fake := newFakeFS()
	fake.blockReadsOf("hello.txt")
	target, _ := mountFake(t, fake, defaultSettings(), nil)

	// Direct I/O makes the kernel wait for the answer to this very request, which is what
	// makes it interruptible. A buffered read is served by read-ahead requests that run on
	// after the reader is gone and are never interrupted.
	cmd := exec.CommandContext(testContext(t), "dd",
		"if="+filepath.Join(target, "hello.txt"), "of=/dev/null", "bs=4096", "count=1", "iflag=direct")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.reading:
	case <-time.After(waitTimeout):
		killAndReap(cmd)
		t.Fatal("the read never reached the core")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	reap(cmd)

	select {
	case err := <-fake.cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("core saw %v, want context.Canceled", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("killing the reader did not cancel the core call")
	}
}

// The Go syscall package offers extended attributes on Linux only.
// The Linux kernel remembers that a name does not exist for as long as the client tells it
// to. FreeBSD's FUSE asks again at every look-up, so this is checked on Linux only.
func TestMissingNamesAreRemembered(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	missing := filepath.Join(target, "nope")

	if _, err := os.Stat(missing); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("Stat = %v, want ENOENT", err)
	}
	asked := fake.statCalls.Load()
	if _, err := os.Stat(missing); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("second Stat = %v, want ENOENT", err)
	}
	if got := fake.statCalls.Load(); got != asked {
		t.Fatalf("a remembered missing name reached the core again (%d calls, then %d)", asked, got)
	}
}

func TestSetxattrIsRefusedWithEROFS(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	hello := filepath.Join(target, "hello.txt")
	if err := syscall.Setxattr(hello, "user.x", []byte("v"), 0); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("setxattr = %v, want EROFS", err)
	}
	if got, err := os.ReadFile(hello); err != nil || string(got) != fakeContent {
		t.Fatalf("file changed: %q, %v", got, err)
	}
}

func TestExtendedAttributesAreNotSupported(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	hello := filepath.Join(target, "hello.txt")

	if _, err := syscall.Getxattr(hello, "user.x", make([]byte, 8)); !errors.Is(err, syscall.ENOTSUP) {
		t.Errorf("getxattr = %v, want ENOTSUP", err)
	}
	if _, err := syscall.Listxattr(hello, make([]byte, 8)); !errors.Is(err, syscall.ENOTSUP) {
		t.Errorf("listxattr = %v, want ENOTSUP", err)
	}
}

// Linux has a lazy unmount that leaves open files working. FreeBSD only has a forced
// unmount, which makes open files fail.
func TestForcedUnmountDetachesAndOpenFilesKeepWorking(t *testing.T) {
	fake := newFakeFS()
	target, mnt := mountFake(t, fake, defaultSettings(), nil)
	file, err := os.Open(filepath.Join(target, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(file)

	if err = mnt.Unmount(testContext(t), true); err != nil {
		t.Fatalf("forced Unmount = %v", err)
	}
	if _, ok := findMount(t, target); ok {
		t.Fatal("forced unmount left the mount in the tree")
	}
	select {
	case <-mnt.Done():
		t.Fatal("mount ended while a file is still open")
	default:
	}

	got, err := io.ReadAll(file)
	if err != nil || string(got) != fakeContent {
		t.Fatalf("open file after detach = %q, %v", got, err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, mnt.Done())
	requireNoLeftovers(t, mnt, fake)
}

func requireNoLeftovers(t *testing.T, mnt mount.Mounted, fake *fakeFS) {
	t.Helper()
	if mnt.Err() != nil {
		t.Fatalf("Err = %v, want nil", mnt.Err())
	}
	if got := fake.open.Load(); got != 0 {
		t.Fatalf("%d files still open in the core", got)
	}
}

// crashedClient mounts a fake in a child process on target and kills the child, leaving
// the dead mount a crashed client leaves.
func crashedClient(t *testing.T, target string) {
	t.Helper()
	ctx := testContext(t)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashEnv+"="+target)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		killAndReap(cmd)
		t.Fatalf("client did not become ready: %q, %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	reap(cmd)
}

// killAndReap ends a child that a test gives up on. Both steps may fail because the child
// is already gone, and the test is about to fail for another reason.
func killAndReap(cmd *exec.Cmd) {
	_ = cmd.Process.Kill() //nolint:errcheck // the child may be gone already, and a test is about to fail anyway
	reap(cmd)
}

// reap waits for a child that was killed. The wait always reports the kill as an error.
func reap(cmd *exec.Cmd) {
	_ = cmd.Wait() //nolint:errcheck // a killed child always reports the kill as its exit status
}

// FreeBSD has no control directory to abort a connection with, and what it does with the
// mount of a dead client is not the same as ENOTCONN.
func TestDeadMountFromACrashedClientIsClearedAtStart(t *testing.T) {
	skipWithoutFuse(t)
	target := t.TempDir()
	crashedClient(t, target)
	if _, err := os.Stat(target); !errors.Is(err, syscall.ENOTCONN) {
		t.Fatalf("Stat of the crashed client's mount = %v, want ENOTCONN", err)
	}

	m, err := Mount(newFakeFS(), mount.Options{Target: target, Source: testSource})
	if err != nil {
		t.Fatalf("Mount over a dead mount: %v", err)
	}
	t.Cleanup(func() { endMount(t, m) })
	if got, err := os.ReadFile(filepath.Join(target, "hello.txt")); err != nil || string(got) != fakeContent {
		t.Fatalf("read through the new mount = %q, %v", got, err)
	}
}
