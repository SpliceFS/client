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
		msg := strings.ToLower(end.stderr)
		if strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied") {
			t.Skipf("this user cannot mount without root: %s", end.stderr)
		}
		t.Fatalf("mount ended before it was ready: %+v", end)
	}
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
