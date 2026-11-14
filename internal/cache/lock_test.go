package cache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holderTimeout ends a second process that a test forgot or failed to stop.
const holderTimeout = time.Minute

// holderEnv names the cache folder that the test binary opens and holds when it is started
// as a second process.
const holderEnv = "SPLICEFS_CACHE_TEST_HOLD_FOLDER"

// TestMain doubles as the second process of the lock tests: when the environment names a
// folder, the test binary opens a cache there, says so, and waits to be killed. It never
// closes the cache, so the only thing that frees the lock is the end of the process.
func TestMain(m *testing.M) {
	if dir := os.Getenv(holderEnv); dir != "" {
		os.Exit(holdFolder(dir))
	}
	m.Run()
}

func holdFolder(dir string) int {
	if _, err := Open(Options{Dir: dir, MaxSize: math.MaxInt64, MaxAge: math.MaxInt64}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 2
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready") //nolint:errcheck // the parent fails the test when it reads nothing
	_, _ = io.Copy(io.Discard, os.Stdin)    //nolint:errcheck // the parent ends this process, an error changes nothing
	return 0
}

// startHolder starts a process that holds the cache folder dir and returns it once it does.
func startHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), holderTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holderEnv+"="+dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(cmd) })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the second process did not open the folder: %q, %v, %s", line, err, stderr.String())
	}
	return cmd
}

// stop ends the process the way a crash does and waits for it. Ending one that is gone
// already is not an error.
func stop(cmd *exec.Cmd) {
	_ = cmd.Process.Kill() //nolint:errcheck // the process may be gone already
	_ = cmd.Wait()         //nolint:errcheck // a killed process always reports the kill as its exit status
}

func TestSecondOpenOfAHeldFolderFailsAndTouchesNothing(t *testing.T) {
	t.Parallel()
	setup := newEnv(t)
	setup.open()
	// What a scan would remove: a data file whose metadata file is missing.
	hash := strings.Repeat("ab", hashLen/2)
	orphan := filepath.Join(setup.opts.Dir, hash[:shardLen], hash+dataExt)
	if err := os.MkdirAll(filepath.Dir(orphan), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}

	second, err := Open(setup.opts)
	if err == nil {
		closeCache(t, second)
		t.Fatal("the second open of the folder succeeded")
	}
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), setup.opts.Dir) {
		t.Errorf("error = %v, want ErrInUse naming the folder %s", err, setup.opts.Dir)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("the refused open touched the folder: %v", err)
	}
}

func TestLockIsFreeAfterClose(t *testing.T) {
	t.Parallel()
	setup := newEnv(t)
	first := setup.open()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	setup.open()
}

func TestFolderHeldByAnotherProcessIsRefusedUntilItEnds(t *testing.T) {
	t.Parallel()
	setup := newEnv(t)
	holder := startHolder(t, setup.opts.Dir)

	other, err := Open(setup.opts)
	if err == nil {
		closeCache(t, other)
		t.Fatal("the open succeeded while another process held the folder")
	}
	if !errors.Is(err, ErrInUse) {
		t.Errorf("error = %v, want ErrInUse", err)
	}
	stop(holder)
	setup.open()
}

func TestFailedOpenGivesTheLockBack(t *testing.T) {
	t.Parallel()
	setup := newEnv(t)
	if c, err := openWith(setup.opts, faultFS{fail: failOn("readdir", "cache")}, syscall.Flock); err == nil {
		closeCache(t, c)
		t.Fatal("open succeeded although the folder cannot be read")
	}
	setup.open()
}

func TestLockFileThatCannotBeOpenedFailsTheOpen(t *testing.T) {
	t.Parallel()
	setup := newEnv(t)
	// A folder where the lock file belongs.
	if err := os.MkdirAll(filepath.Join(setup.opts.Dir, lockName), dirPerm); err != nil {
		t.Fatal(err)
	}
	c, err := Open(setup.opts)
	if err == nil {
		closeCache(t, c)
		t.Fatal("open succeeded without a lock file")
	}
	if errors.Is(err, ErrInUse) {
		t.Errorf("error = %v, must not claim another process holds the folder", err)
	}
}

func TestLockCallThatFailsIsNotReportedAsBusy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file, err := lockFolder(dir, func(int, int) error { return errInjected })
	if err == nil {
		_ = file.Close() //nolint:errcheck // the test fails anyway
		t.Fatal("lockFolder succeeded although the lock call failed")
	}
	if !errors.Is(err, errInjected) || errors.Is(err, ErrInUse) {
		t.Errorf("error = %v, want the failure of the lock call and not ErrInUse", err)
	}
}
