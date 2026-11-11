package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fakeSystem(env map[string]string, euid int) system {
	sys := systemHost()
	sys.getenv = func(key string) string { return env[key] }
	sys.geteuid = func() int { return euid }
	sys.tempDir = func() string { return "/tmp/example" }
	sys.abs = func(p string) (string, error) {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		return filepath.Join("/work", p), nil
	}
	return sys
}

func TestSocketPath(t *testing.T) {
	t.Parallel()
	sum := func(dir string) string {
		h := sha256.Sum256([]byte(dir))
		return hex.EncodeToString(h[:8])
	}
	tests := []struct {
		name string
		sys  system
		dir  string
		want string
	}{
		{"runtime directory", fakeSystem(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, 1000), "/etc/splicefs/client", "/run/user/1000/splicefs/" + sum("/etc/splicefs/client") + ".sock"},
		{"root without runtime directory", fakeSystem(nil, 0), "/etc/splicefs/client", "/run/splicefs/" + sum("/etc/splicefs/client") + ".sock"},
		{"ordinary user without runtime directory", fakeSystem(nil, 1000), "/etc/splicefs/client", "/tmp/example/splicefs/" + sum("/etc/splicefs/client") + ".sock"},
		{"relative directory is made absolute", fakeSystem(nil, 0), "conf/", "/run/splicefs/" + sum("/work/conf") + ".sock"},
		{"unresolvable directory is cleaned", func() system {
			s := fakeSystem(nil, 0)
			s.abs = func(string) (string, error) { return "", errors.New("no cwd") }
			return s
		}(), "/etc//x/", "/run/splicefs/" + sum("/etc/x") + ".sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.sys.socketPath(tt.dir); got != tt.want {
				t.Errorf("socketPath(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestSocketPathUsesTheEnvironment(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")
	abs, err := filepath.Abs("conf")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := SocketPath("conf"), fakeSystem(map[string]string{"XDG_RUNTIME_DIR": "/run/user/4242"}, 1).socketPath(abs); got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestSocketPathDiffersPerConfiguration(t *testing.T) {
	t.Parallel()
	sys := fakeSystem(nil, 0)
	if sys.socketPath("/a") == sys.socketPath("/b") {
		t.Error("two configurations share a socket")
	}
	if sys.socketPath("/a") != sys.socketPath("/a/") {
		t.Error("one configuration has two sockets")
	}
}

// answers reports whether something accepts connections on the socket at path.
func answers(path string) bool {
	conn, err := dialUnix(path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestClaimCreatesAPrivateSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "run", "c.sock")
	sock, err := systemHost().claim(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&fs.ModeSocket == 0 {
		t.Errorf("socket mode = %v, want a socket with 600", info.Mode())
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %o, want 700", dir.Mode().Perm())
	}
	if !answers(path) {
		t.Error("nothing answers on the socket")
	}
	for range 2 {
		if err := sock.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket file after Close: %v", err)
	}
}

// staleSocket leaves a socket file with nothing behind it, as a crashed client does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal(err)
	}
}

func TestClaimReplacesAStaleSocket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dial func(context.Context, string) (net.Conn, error)
	}{
		{"connection refused", nil},
		{"file vanished", func(context.Context, string) (net.Conn, error) { return nil, fs.ErrNotExist }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(shortDir(t), "c.sock")
			staleSocket(t, path)
			sys := systemHost()
			if tt.dial != nil {
				sys.dial = tt.dial
			}
			sock, err := sys.claim(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sock.Close() }()
			if !answers(path) {
				t.Error("the new socket does not answer")
			}
		})
	}
}

func TestClaimRefusesALiveSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "c.sock")
	first, err := systemHost().claim(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()

	_, err = systemHost().claim(path)
	errIs(t, err, ErrAlreadyRunning)
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the socket: %v", err)
	}
	if !answers(path) {
		t.Error("the running client's socket was removed")
	}
}

func TestClaimFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string, sys *system) string // returns the socket path
		want  string
	}{
		{"directory cannot be created", func(t *testing.T, dir string, _ *system) string {
			file := filepath.Join(dir, "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(file, "sub", "c.sock")
		}, "create socket directory"},
		{"directory cannot be examined", func(_ *testing.T, dir string, sys *system) string {
			sys.lstat = func(string) (fs.FileInfo, error) { return nil, errors.New("io error") }
			return filepath.Join(dir, "c.sock")
		}, "check socket directory: io error"},
		{"directory is a symbolic link", func(t *testing.T, dir string, _ *system) string {
			target := filepath.Join(dir, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(dir, "link", "c.sock")
		}, "is not a directory"},
		{"directory is writable by others", func(t *testing.T, dir string, _ *system) string {
			if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // the test needs an unsafe directory
				t.Fatal(err)
			}
			return filepath.Join(dir, "c.sock")
		}, "can be written by other users"},
		{"path cannot be bound", func(_ *testing.T, dir string, _ *system) string {
			return filepath.Join(dir, strings.Repeat("x", 300)+".sock")
		}, "listen on"},
		{"a leftover cannot be checked", func(t *testing.T, dir string, sys *system) string {
			path := filepath.Join(dir, "c.sock")
			staleSocket(t, path)
			sys.dial = func(context.Context, string) (net.Conn, error) { return nil, syscall.EAGAIN }
			return path
		}, "check for a running client"},
		{"a leftover cannot be removed", func(t *testing.T, dir string, sys *system) string {
			path := filepath.Join(dir, "c.sock")
			if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil {
				t.Fatal(err)
			}
			sys.dial = func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
			return path
		}, "remove stale socket"},
		{"the mode cannot be set", func(_ *testing.T, dir string, sys *system) string {
			sys.chmod = func(string, fs.FileMode) error { return errors.New("not permitted") }
			return filepath.Join(dir, "c.sock")
		}, "restrict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := shortDir(t)
			sys := systemHost()
			path := tt.setup(t, dir, &sys)
			sock, err := sys.claim(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("claim = %v, want an error containing %q", err, tt.want)
			}
			if sock != nil {
				t.Errorf("claim returned a socket with an error")
			}
		})
	}
}

func TestAbsPath(t *testing.T) {
	t.Parallel()
	sys := fakeSystem(nil, 1)
	got, err := sys.absPath("a//b/../c")
	if err != nil || got != "/work/a/c" {
		t.Errorf("absPath = %q, %v", got, err)
	}
	sys.abs = func(string) (string, error) { return "", errors.New("no cwd") }
	if _, err := sys.absPath("x"); err == nil || !strings.Contains(err.Error(), `resolve "x": no cwd`) {
		t.Errorf("absPath = %v", err)
	}
}
