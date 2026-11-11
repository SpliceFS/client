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

const helloText = "hello from the cluster\n"

var helloTime = time.Unix(1_700_000_000, 0)

// helloFS is a filesystem with one file in its root.
type helloFS struct{}

var (
	helloRoot = core.Attr{Dir: true, MTime: helloTime, Inode: 1}
	helloFile = core.Attr{Name: "hello.txt", Size: int64(len(helloText)), MTime: helloTime, Inode: 2}
)

func (helloFS) Stat(_ context.Context, name string) (core.Attr, error) {
	switch name {
	case "":
		return helloRoot, nil
	case helloFile.Name:
		return helloFile, nil
	}
	return core.Attr{}, core.ErrNotExist
}

func (helloFS) ReadDir(_ context.Context, name string) ([]core.Attr, error) {
	if name != "" {
		return nil, core.ErrNotExist
	}
	return []core.Attr{helloFile}, nil
}

func (helloFS) Open(_ context.Context, name string) (core.File, error) {
	if name != helloFile.Name {
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

func (helloOpen) Attr() core.Attr { return helloFile }
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

// TestRealMount runs the program against the real NFS mount layer of this Mac: mount,
// read a file through the mount, status, umount. It is skipped when the system does not
// let this user mount, and never uses sudo.
func TestRealMount(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mnt")
	for path, content := range map[string]string{
		filepath.Join(dir, "password"): "secret\n",
		filepath.Join(dir, "config.yaml"): "cache:\n  dir: " + filepath.Join(dir, "cache") + "\n" +
			"clusters:\n  example:\n    url: https://files.example.com/api\n    user: alice\n    password_file: password\n" +
			"mounts:\n  - cluster: example\n    path: /\n    target: " + target + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(target, 0o755); err != nil { //nolint:gosec // a mount target must be traversable
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Whatever happened, nothing may stay mounted.
		_ = exec.CommandContext(context.Background(), "/sbin/umount", "-f", target).Run()
	})

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
	go func() {
		var out, errOut bytes.Buffer
		status := execute(context.Background(), []string{"--config", dir, "mount"}, &out, &errOut, b)
		ended <- result{status: status, stdout: out.String(), stderr: errOut.String()}
	}()
	select {
	case <-ready:
	case end := <-ended:
		msg := strings.ToLower(end.stderr)
		if strings.Contains(msg, "not permitted") || strings.Contains(msg, "permission denied") {
			t.Skipf("this user cannot mount without root: %s", end.stderr)
		}
		t.Fatalf("mount ended before it was ready: %+v", end)
	}

	got, err := os.ReadFile(filepath.Join(target, "hello.txt"))
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
	if end := <-ended; end.status != exitOK {
		t.Errorf("mount ended with %+v, want a clean exit", end)
	}
	if table := mountTable(t); strings.Contains(table, "splicefs:example") {
		t.Errorf("a splicefs mount is left:\n%s", table)
	}
}
