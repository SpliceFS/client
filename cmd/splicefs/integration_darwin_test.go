//go:build darwin

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
)

const (
	helloText = "hello from the cluster\n"
	helloName = "hello.txt"
	// helloUnix is the modification time of every entry, a fixed second so that runs compare equal.
	helloUnix = 1_700_000_000
)

// helloRoot is the attributes of the root folder of helloFS.
func helloRoot() core.Attr {
	return core.Attr{Dir: true, MTime: time.Unix(helloUnix, 0), Inode: 1}
}

// helloFile is the attributes of the one file in helloFS.
func helloFile() core.Attr {
	return core.Attr{Name: helloName, Size: int64(len(helloText)), MTime: time.Unix(helloUnix, 0), Inode: 2}
}

// helloFS is a filesystem with one file in its root.
type helloFS struct{}

func (helloFS) Stat(_ context.Context, name string) (core.Attr, error) {
	switch name {
	case "":
		return helloRoot(), nil
	case helloName:
		return helloFile(), nil
	}
	return core.Attr{}, core.ErrNotExist
}

func (helloFS) ReadDir(_ context.Context, name string) ([]core.Attr, error) {
	if name != "" {
		return nil, core.ErrNotExist
	}
	return []core.Attr{helloFile()}, nil
}

//nolint:ireturn // it implements FS.Open, which returns the File interface.
func (helloFS) Open(_ context.Context, name string) (core.File, error) {
	if name != helloName {
		return nil, core.ErrNotExist
	}
	return helloOpen{}, nil
}

func (helloFS) StatFS(context.Context) (core.Space, error) {
	return core.Space{Total: 1 << 30, Free: 1 << 29}, nil
}

type helloOpen struct{}

func (helloOpen) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(helloText)) {
		return 0, io.EOF
	}
	n := copy(p, helloText[off:])
	if off+int64(n) == int64(len(helloText)) {
		return n, io.EOF
	}
	return n, nil
}

func (helloOpen) Attr() core.Attr { return helloFile() }
func (helloOpen) Close() error    { return nil }

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

// realMount is a mount command running in the background.
type realMount struct {
	// ready is closed when the mount is ready.
	ready <-chan struct{}
	// ended receives the result when the command ends.
	ended <-chan result
	// stop ends the command, the way a stop signal does.
	stop func()
}

// startRealMount runs the mount command in the background with helloFS behind it.
func startRealMount(t *testing.T, dir string) realMount {
	t.Helper()
	ready := make(chan struct{})
	b := systemBackend()
	realRun := b.run
	b.run = func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
		opts.NewCore = func(*config.Config, core.Options) (app.Core, error) { return stubCore{fsys: helloFS{}}, nil }
		return realRun(ctx, cfg, targets, opts)
	}
	b.notify = func(state string) error {
		if state == "READY=1" {
			close(ready)
		}
		return nil
	}
	ended := make(chan result, 1)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() {
		var out, errOut bytes.Buffer
		status := execute(ctx, []string{"--config", dir, "mount"}, &out, &errOut, b)
		ended <- result{status: status, stdout: out.String(), stderr: errOut.String()}
	}()
	return realMount{ready: ready, ended: ended, stop: stop}
}

// awaitReady waits until the mount is ready. It skips the test when the mount ended because
// this user may not mount, and fails it when the mount ended for any other reason.
func awaitReady(t *testing.T, mounted realMount) {
	t.Helper()
	select {
	case <-mounted.ready:
	case end := <-mounted.ended:
		skipOrFailEnded(t, end)
	}
}

// skipOrFailEnded handles a mount that ended before it was ready: it skips the test when
// the reason is that this user may not mount, and fails it otherwise.
func skipOrFailEnded(t *testing.T, end result) {
	t.Helper()
	msg := strings.ToLower(end.stderr)
	if strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied") {
		t.Skipf("this user cannot mount without root: %s", end.stderr)
	}
	t.Fatalf("mount ended before it was ready: %+v", end)
}

// TestRealMount runs the program against the real NFS mount layer of this Mac: mount,
// read a file through the mount, status, umount. It is skipped when the system does not
// let this user mount, and never uses sudo.
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

	mounted := startRealMount(t, dir)
	awaitReady(t, mounted)

	got, err := os.ReadFile(filepath.Join(target, helloName))
	if err != nil || string(got) != helloText {
		t.Fatalf("read through the mount = %q, %v", got, err)
	}

	f := &fullStack{t: t, configDir: dir}
	status := f.command("status")
	if status.status != exitOK || !strings.Contains(status.stdout, target) || !strings.Contains(status.stdout, "mounted") {
		t.Errorf("status: %+v", status)
	}
	if umount := f.command("umount"); umount.status != exitOK {
		t.Errorf("umount: %+v", umount)
	}
	// The client keeps running after its last mount is gone, until it is stopped.
	mounted.stop()
	if end := <-mounted.ended; end.status != exitOK {
		t.Errorf("mount ended with %+v, want a clean exit", end)
	}
	if table := mountTable(t); strings.Contains(table, "splicefs:example") {
		t.Errorf("a splicefs mount is left:\n%s", table)
	}
}

// clientEnv marks the child process that TestMain runs as the program.
const clientEnv = "SPLICEFS_TEST_CLIENT_PROCESS"

// TestMain doubles as the program for the tests that must kill it: when the environment
// marks the process as the client, the test binary runs the command line it was given
// against helloFS and ends with the exit status of the command.
func TestMain(m *testing.M) {
	if os.Getenv(clientEnv) != "" {
		os.Exit(runClientProcess())
	}
	m.Run()
}

func runClientProcess() int {
	b := systemBackend()
	realRun := b.run
	b.run = func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
		opts.NewCore = func(*config.Config, core.Options) (app.Core, error) { return stubCore{fsys: helloFS{}}, nil }
		return realRun(ctx, cfg, targets, opts)
	}
	return execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr, b)
}

// client is the program running in a child process.
type client struct {
	process *os.Process
	// ended receives the result when the process has ended.
	ended <-chan result
}

// startClient runs "splicefs --config dir command" in a child process. The process is killed
// when the test ends.
func startClient(t *testing.T, dir, command string) client {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), os.Args[0], "--config", dir, command)
	cmd.Env = append(os.Environ(), clientEnv+"=1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() }) //nolint:errcheck // it fails when the process has ended, which is fine
	ended := make(chan result, 1)
	go func() {
		_ = cmd.Wait() //nolint:errcheck // the exit status is read from the process state
		ended <- result{status: cmd.ProcessState.ExitCode(), stdout: out.String(), stderr: errOut.String()}
	}()
	return client{process: cmd.Process, ended: ended}
}

// runClient runs one command of the program in a child process and returns its result.
// It fails the test when the command takes longer than limit.
func runClient(t *testing.T, dir, command string, limit time.Duration) result {
	t.Helper()
	select {
	case end := <-startClient(t, dir, command).ended:
		return end
	case <-time.After(limit):
		t.Fatalf("%s did not end within %s", command, limit)
		return result{}
	}
}

// isListed reports whether the mount table lists a mount on target. It never touches target
// itself, which blocks for many seconds when the mount is stale.
func isListed(t *testing.T, target string) bool {
	t.Helper()
	return strings.Contains(mountTable(t), " on "+target+" (")
}

// awaitListed waits until the mount table lists a mount on target, and skips the test when the
// client ended because this user may not mount.
func awaitListed(t *testing.T, target string, ended <-chan result) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case end := <-ended:
			skipOrFailEnded(t, end)
		default:
		}
		if isListed(t, target) {
			return
		}
	}
	t.Fatalf("the mount table does not list %s:\n%s", target, mountTable(t))
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
// that its mount is left behind pointing at a dead port, and checks that mount refuses the
// target and that umount removes the mount. Nothing in it reads, stats or lists the target
// while the mount is stale, because that blocks for many seconds. It is skipped when the
// system does not let this user mount.
//
//nolint:paralleltest // it changes the system mount table and checks it
func TestRealMountLeftBehindByAKilledClientIsRemovedByUmount(t *testing.T) {
	dir := t.TempDir()
	place := prepareTarget(t, dir)

	running := startClient(t, dir, "mount")
	awaitListed(t, place.listed, running.ended)
	if err := running.process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-running.ended
	if !isListed(t, place.listed) {
		t.Fatal("the mount is gone after the client was killed, so there is no stale mount to test")
	}

	const limit = 10 * time.Second
	refused := runClient(t, dir, "mount", limit)
	requireOutput(t, refused, exitError, "is still mounted by a client that is no longer running", "splicefs umount")
	requireOutput(t, runClient(t, dir, "umount", limit), exitOK, "removed the mount", place.target)
	if isListed(t, place.listed) {
		t.Error("umount left the mount in the mount table")
	}

	requireOutput(t, runClient(t, dir, "umount", limit), exitError, "no client is running")
}
