package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holderTimeout ends a second process that a test forgot or failed to stop.
const holderTimeout = time.Minute

// holderEnv is set to 1 when the test binary is started as a second process.
const holderEnv = "SPLICEFS_APP_TEST_HOLD_SOCKET"

// holderSocket is the name of the socket that the second process claims, in its working
// directory. The directory comes from the parent as the working directory and not as a
// text the process reads, so that the linter does not see a path that arrives from the
// environment reach the file calls of the production code.
const holderSocket = "c.sock"

// TestMain doubles as the second process of the lock test: when the environment says so,
// the test binary claims the socket in its working directory, says so, and waits until its
// input ends. Its input is a pipe the parent keeps open, so it waits until the parent kills
// it or the parent itself ends. It never closes the socket, so the only thing that frees the
// lock is the end of the process, and the socket file stays behind as it does after a crash.
func TestMain(m *testing.M) {
	if os.Getenv(holderEnv) == "1" {
		os.Exit(holdSocketHere())
	}
	m.Run()
}

func holdSocketHere() int {
	if _, err := systemHost().claim(context.Background(), holderSocket); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 2
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready") //nolint:errcheck // the parent fails the test when it reads nothing
	_, _ = io.Copy(io.Discard, os.Stdin)    //nolint:errcheck // ends when the parent kills this process or ends
	return 0
}

// startHolder starts a process that holds the control socket holderSocket in the directory
// dir and returns it once it does.
func startHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), holderTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), holderEnv+"=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Without a pipe the second process reads an empty input, ends right after "ready" and
	// frees the socket, and the test then depends on how fast that exit is. The parent's end
	// of the pipe stays open until Wait in stop.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(cmd) })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the second process did not claim the socket: %q, %v, %s", line, err, stderr.String())
	}
	return cmd
}

// stop ends the process the way a crash does and waits for it. Ending one that is gone
// already is not an error.
func stop(cmd *exec.Cmd) {
	_ = cmd.Process.Kill() //nolint:errcheck // the process may be gone already
	_ = cmd.Wait()         //nolint:errcheck // a killed process always reports the kill as its exit status
}

// Not parallel: starting a process copies every open file of the test program into the
// child until the child runs its own program, the lock files of other tests included, so a
// test running at the same moment could find its own socket still locked. Go runs this test
// before the parallel ones start.
//
//nolint:paralleltest // see above
func TestSocketHeldByAnotherProcessIsRefusedAndTakenOverWhenItCrashes(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, holderSocket)
	holder := startHolder(t, dir)

	_, err := systemHost().claim(t.Context(), path)
	errIs(t, err, ErrAlreadyRunning)
	if !answers(path) {
		t.Error("the refused claim removed the socket of the running client")
	}

	stop(holder)
	// The crash left the socket file behind. Nothing answers on it, and the kernel dropped
	// the lock with the process.
	if answers(path) {
		t.Fatal("something still answers on the socket of the ended process")
	}
	sock, err := systemHost().claim(t.Context(), path)
	if err != nil {
		t.Fatalf("claim after the holder ended: %v", err)
	}
	defer closeQuietly(sock)
	if !answers(path) {
		t.Error("the new socket does not answer")
	}
}
