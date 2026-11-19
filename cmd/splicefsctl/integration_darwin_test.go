//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/internal/cli"
	"github.com/SpliceFS/client/internal/clienttest"
	"github.com/SpliceFS/client/mount/nfs"
)

// The tests in this file run the client for real, with the NFS mount layer of this Mac and
// a filesystem with one file behind it, and use splicefsctl against it. They are skipped when
// the system does not let this user mount, and never use sudo.

// mountTable returns what the system reports as mounted.
func mountTable(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "/sbin/mount").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// writeMountConfig writes a configuration folder in dir with one cluster and one mount at target.
func writeMountConfig(t *testing.T, dir, target string) {
	t.Helper()
	cluster := "url: https://files.example.com/api\nuser: alice\npassword_file: password\n"
	cache := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\n"
	mountFile := "cluster: example\npath: /\ntarget: " + target + "\n"
	for path, content := range map[string]string{
		filepath.Join(dir, "password"):                   "secret\n",
		filepath.Join(dir, "conf.d", "cache.yaml"):       cache,
		filepath.Join(dir, "clusters.d", "example.yaml"): cluster,
		filepath.Join(dir, "mounts.d", "main.yaml"):      mountFile,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// serve runs the client of the configuration folder dir, the way the splicefs program does,
// with the filesystem of the tests behind its mounts. It calls ready when the mounts are up
// and returns when ctx ends or the client fails.
func serve(ctx context.Context, dir string, ready func()) error {
	cfg, err := config.LoadDir(dir)
	if err != nil {
		return err
	}
	return app.Run(ctx, cfg, nil, app.Options{
		ConfigDir: dir,
		Mounter:   nfs.Mount,
		NewCore: func(*config.Config, core.Options) (app.Core, error) {
			return clienttest.Core{FS: clienttest.HelloFS{}}, nil
		},
		Notify: func(state string) error {
			if state == "READY=1" {
				ready()
			}
			return nil
		},
	})
}

// realClient is a running client.
type realClient struct {
	// ready is closed when the mounts are up.
	ready <-chan struct{}
	// ended receives the error of the client when it ends.
	ended <-chan error
	// stop ends the client, the way a stop signal does.
	stop func()
}

// startRealClient runs the client of dir in the background.
func startRealClient(t *testing.T, dir string) realClient {
	t.Helper()
	ready := make(chan struct{})
	ended := make(chan error, 1)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { ended <- serve(ctx, dir, func() { close(ready) }) }()
	return realClient{ready: ready, ended: ended, stop: stop}
}

// awaitReady waits until the mounts are up. It skips the test when the client ended because
// this user may not mount, and fails it when the client ended for any other reason.
func awaitReady(t *testing.T, running realClient) {
	t.Helper()
	select {
	case <-running.ready:
	case err := <-running.ended:
		skipOrFailEnded(t, err.Error())
	}
}

// skipOrFailEnded handles a client that ended before its mounts were up: it skips the test
// when the reason is that this user may not mount, and fails it otherwise.
func skipOrFailEnded(t *testing.T, reason string) {
	t.Helper()
	if userMayNotMount(reason) {
		t.Skipf("this user cannot mount without root: %s", reason)
	}
	t.Fatalf("the client ended before it was ready: %s", reason)
}

// userMayNotMount reports whether what a client said when it ended means that this user may not mount.
func userMayNotMount(reason string) bool {
	msg := strings.ToLower(reason)
	return strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied")
}

// command runs a splicefsctl command against the client of dir and returns what it produced.
func command(ctx context.Context, dir string, args ...string) result {
	var out, errOut bytes.Buffer
	argv := append([]string{"--config", dir}, args...)
	status := execute(ctx, argv, &out, &errOut, systemBackend())
	return result{status: status, stdout: out.String(), stderr: errOut.String()}
}

// TestRealMount runs splicefsctl against the real NFS mount layer of this Mac: the client
// mounts, a file is read through the mount, status, umount.
//
//nolint:paralleltest // it changes the system mount table and checks it at the end
func TestRealMount(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mnt")
	writeMountConfig(t, dir, target)
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Whatever happened, nothing may stay mounted. The unmount fails when the test already
		// did it, and the check of the mount table at the end of the test reports a real leftover.
		_ = exec.CommandContext(context.Background(), "/sbin/umount", "-f", target).Run() //nolint:errcheck // see above
	})

	running := startRealClient(t, dir)
	awaitReady(t, running)

	got, err := os.ReadFile(filepath.Join(target, clienttest.HelloName))
	if err != nil || string(got) != clienttest.HelloText {
		t.Fatalf("read through the mount = %q, %v", got, err)
	}

	status := command(context.Background(), dir, "status")
	requireOutput(t, status, cli.ExitOK, target, "mounted")
	if umount := command(context.Background(), dir, "umount"); umount.status != cli.ExitOK {
		t.Errorf("umount: %+v", umount)
	}
	// The client keeps running after its last mount is gone, until it is stopped.
	running.stop()
	if err := <-running.ended; err != nil {
		t.Errorf("the client ended with %v, want a clean end", err)
	}
	if table := mountTable(t); strings.Contains(table, "splicefs:example") {
		t.Errorf("a splicefs mount is left:\n%s", table)
	}
}

// clientEnv marks the child process that TestMain runs as the client.
const clientEnv = "SPLICEFS_TEST_CLIENT_DIR"

// readyFD is the descriptor of the child on which it writes one byte when its mounts are up:
// the first of cmd.ExtraFiles, after stdin, stdout and stderr.
const readyFD = 3

// TestMain doubles as the client for the test that must kill it: when the environment names a
// configuration folder, the test binary runs the client of that folder, writes one byte on
// readyFD when its mounts are up, and ends with an error status when the client fails.
func TestMain(m *testing.M) {
	if dir := os.Getenv(clientEnv); dir != "" {
		ready := os.NewFile(readyFD, "ready")
		err := serve(context.Background(), dir, func() {
			_, _ = ready.Write([]byte{1}) //nolint:errcheck // a parent that is gone has nothing to be told
		})
		if err != nil {
			// A failed write has nowhere to be reported, stderr is where it would go; the status
			// below tells the parent that the client failed.
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(cli.ExitError)
		}
		os.Exit(cli.ExitOK)
	}
	m.Run()
}

// childClient is the client running in a child process.
type childClient struct {
	process *os.Process
	// ready is closed when the child said its mounts are up.
	ready <-chan struct{}
	// ended is closed when the process has ended.
	ended <-chan struct{}
	// stderr is what the child wrote to stderr; read it only after ended is closed.
	stderr *bytes.Buffer
}

// startChildClient runs the client of dir in a child process. The process is killed when the
// test ends.
func startChildClient(t *testing.T, dir string) childClient {
	t.Helper()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), os.Args[0])
	cmd.Env = append(os.Environ(), clientEnv+"="+dir)
	cmd.ExtraFiles = []*os.File{readyWrite}
	cmd.Stderr = &stderr
	err = cmd.Start()
	// The child has its own copy now; closing this one lets the read end see the end of the
	// child as the end of the pipe.
	_ = readyWrite.Close() //nolint:errcheck // closing a pipe end that was never written cannot lose data
	if err != nil {
		_ = readyRead.Close() //nolint:errcheck // see above
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() }) //nolint:errcheck // it fails when the process has ended, which is fine
	ready := make(chan struct{})
	go func() {
		defer readyRead.Close() //nolint:errcheck // a read end has nothing to flush
		if _, err := readyRead.Read(make([]byte, 1)); err == nil {
			close(ready)
		}
	}()
	ended := make(chan struct{})
	go func() {
		_ = cmd.Wait() //nolint:errcheck // only the end of the process and its stderr matter here
		close(ended)
	}()
	return childClient{process: cmd.Process, ready: ready, ended: ended, stderr: &stderr}
}

// isListed reports whether the mount table lists a mount on target. It never touches target
// itself, which blocks for many seconds when the mount is stale.
func isListed(t *testing.T, target string) bool {
	t.Helper()
	return strings.Contains(mountTable(t), " on "+target+" (")
}

// awaitChildReady waits until the child says its mounts are up. It skips the test when the
// child ended because this user may not mount, and fails it when it ended for another reason.
func awaitChildReady(t *testing.T, running childClient) {
	t.Helper()
	select {
	case <-running.ready:
	case <-running.ended:
		skipOrFailEnded(t, running.stderr.String())
	}
}

func TestUserMayNotMount(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"mount_nfs: can't mount / on /Users/a/mnt: Operation not permitted":   true,
		"mount_nfs: mount on /Users/a/mnt: Permission denied":                 true,
		"mount_nfs: can't mount with remote locks when server is not running": false,
		"": false,
	}
	for said, want := range tests {
		if got := userMayNotMount(said); got != want {
			t.Errorf("userMayNotMount(%q) = %v, want %v", said, got, want)
		}
	}
}

// TestAwaitChildReadySkipsWhenTheChildSaidItMayNotMount checks that a child that ended before its
// mounts were up is judged by what it wrote to stderr, which is where the reason is.
func TestAwaitChildReadySkipsWhenTheChildSaidItMayNotMount(t *testing.T) {
	t.Parallel()
	ended := make(chan struct{})
	close(ended)
	said := "mount_nfs: can't mount / on /Users/a/mnt: Operation not permitted\n"
	running := childClient{ended: ended, stderr: bytes.NewBufferString(said)}

	var inner *testing.T
	t.Run("child that may not mount", func(t *testing.T) {
		inner = t
		awaitChildReady(t, running)
		t.Error("awaitChildReady returned for a child that ended")
	})
	if !inner.Skipped() {
		t.Error("the test was not skipped for a child that said it may not mount")
	}
}

// requireOutput checks that a command ended with status and that its output has every part of want.
func requireOutput(t *testing.T, got result, status int, want ...string) {
	t.Helper()
	output := got.stdout + got.stderr
	if got.status != status {
		t.Errorf("exit status = %d, want %d: %+v", got.status, status, got)
	}
	for _, part := range want {
		if !strings.Contains(output, part) {
			t.Errorf("the output has no %q: %+v", part, got)
		}
	}
}

// mountPlace is the folder a mount is made on.
type mountPlace struct {
	// target is the folder as the configuration spells it.
	target string
	// listed is the folder as the mount table spells it: with its links resolved.
	listed string
}

// prepareTarget writes a configuration with one mount in dir and makes the folder it mounts
// on. The links are resolved before the mount is there, because afterwards that would touch
// the mount. When the test ends, whatever happened, the target is no longer mounted.
func prepareTarget(t *testing.T, dir string) mountPlace {
	t.Helper()
	target := filepath.Join(dir, "mnt")
	writeMountConfig(t, dir, target)
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed := filepath.Join(resolvedDir, "mnt")
	t.Cleanup(func() {
		// The resolved path is used, because umount would otherwise resolve the links itself
		// and wait on the dead mount. The unmount fails when the test already removed the
		// mount, which is the good case.
		_ = exec.CommandContext(context.Background(), "/sbin/umount", "-f", listed).Run() //nolint:errcheck // see above
	})
	return mountPlace{target: target, listed: listed}
}

// TestRealMountLeftBehindByAKilledClientIsRemovedByUmount kills a real client with SIGKILL, so
// that its mount is left behind pointing at a dead port, and checks that a new client refuses
// the target and that umount removes the mount. Nothing in it reads, stats or lists the target
// while the mount is stale, because that blocks for many seconds. It is skipped when the
// system does not let this user mount.
//
//nolint:paralleltest // it changes the system mount table and checks it
func TestRealMountLeftBehindByAKilledClientIsRemovedByUmount(t *testing.T) {
	dir := t.TempDir()
	place := prepareTarget(t, dir)

	running := startChildClient(t, dir)
	awaitChildReady(t, running)
	if err := running.process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-running.ended
	if !isListed(t, place.listed) {
		t.Fatal("the mount is gone after the client was killed, so there is no stale mount to test")
	}

	// The limit only catches a command that hangs. The system decides how long the removal of a
	// dead NFS mount takes: about 2 seconds, more while other mounts are being made, and the race
	// detector sleeps one second more when a process ends.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	refused := serve(ctx, dir, func() {})
	if refused == nil || !strings.Contains(refused.Error(), "is still mounted by a client that is no longer running") ||
		!strings.Contains(refused.Error(), "splicefsctl umount") {
		t.Errorf("a new client on the left-behind mount = %v, want a refusal that names splicefsctl umount", refused)
	}
	requireOutput(t, command(ctx, dir, "umount"), cli.ExitOK, "removed the mount", place.target)
	if isListed(t, place.listed) {
		t.Error("umount left the mount in the mount table")
	}

	requireOutput(t, command(ctx, dir, "umount"), cli.ExitError, "no client is running")
}
