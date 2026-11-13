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

// fakeWorkDir is the working directory the fake system resolves relative paths against.
const fakeWorkDir = "/work"

func fakeSystem(env map[string]string, euid int) system {
	sys := systemHost()
	sys.getenv = func(key string) string { return env[key] }
	sys.geteuid = func() int { return euid }
	sys.tempDir = func() string { return "/tmp/example" }
	sys.abs = func(p string) (string, error) {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		return filepath.Join(fakeWorkDir, p), nil
	}
	return sys
}

func TestSocketPath(t *testing.T) {
	t.Parallel()
	socketUnder := func(base, dir string) string {
		sum := sha256.Sum256([]byte(dir))
		return base + "/splicefs/" + hex.EncodeToString(sum[:8]) + ".sock"
	}
	const configDir = "/etc/splicefs/client"
	runtimeEnv := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	tests := []struct {
		name string
		sys  system
		dir  string
		want string
	}{
		{
			name: "runtime directory",
			sys:  fakeSystem(runtimeEnv, 1000),
			dir:  configDir,
			want: socketUnder("/run/user/1000", configDir),
		},
		{
			name: "root without runtime directory",
			sys:  fakeSystem(nil, 0),
			dir:  configDir,
			want: socketUnder(rootRuntimeDir, configDir),
		},
		{
			name: "ordinary user without runtime directory",
			sys:  fakeSystem(nil, 1000),
			dir:  configDir,
			want: socketUnder("/tmp/example", configDir),
		},
		{
			name: "relative directory is made absolute",
			sys:  fakeSystem(nil, 0),
			dir:  "conf/",
			want: socketUnder(rootRuntimeDir, fakeWorkDir+"/conf"),
		},
		{
			name: "unresolvable directory is cleaned",
			sys: func() system {
				sys := fakeSystem(nil, 0)
				sys.abs = func(string) (string, error) { return "", errors.New("no cwd") }
				return sys
			}(),
			dir:  "/etc//x/",
			want: socketUnder(rootRuntimeDir, "/etc/x"),
		},
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
	want := fakeSystem(map[string]string{"XDG_RUNTIME_DIR": "/run/user/4242"}, 1).socketPath(abs)
	if got := SocketPath("conf"); got != want {
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
	closeQuietly(conn)
	return true
}

// wantPrivateSocket fails the test unless path is a socket only its owner can use, in a folder only its owner can use.
func wantPrivateSocket(t *testing.T, path string) {
	t.Helper()
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
}

func TestClaimCreatesAPrivateSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "run", "c.sock")
	sock, err := systemHost().claim(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	wantPrivateSocket(t, path)
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
			sock, err := sys.claim(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(sock)
			if !answers(path) {
				t.Error("the new socket does not answer")
			}
		})
	}
}

func TestClaimRefusesALiveSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "c.sock")
	first, err := systemHost().claim(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(first)

	_, err = systemHost().claim(t.Context(), path)
	errIs(t, err, ErrAlreadyRunning)
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the socket: %v", err)
	}
	if !answers(path) {
		t.Error("the running client's socket was removed")
	}
}

// claimSetup prepares a failing claim in a fresh directory and returns the socket path to claim.
type claimSetup func(t *testing.T, dir string, sys *system) string

func setupDirectoryUnderAFile(t *testing.T, dir string, _ *system) string {
	t.Helper()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, "sub", "c.sock")
}

func setupDirectoryCannotBeExamined(t *testing.T, dir string, sys *system) string {
	t.Helper()
	sys.lstat = func(string) (fs.FileInfo, error) { return nil, errors.New("io error") }
	return filepath.Join(dir, "c.sock")
}

func setupDirectoryIsALink(t *testing.T, dir string, _ *system) string {
	t.Helper()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "link", "c.sock")
}

func setupDirectoryWritableByOthers(t *testing.T, dir string, _ *system) string {
	t.Helper()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "c.sock")
}

func setupPathTooLong(t *testing.T, dir string, _ *system) string {
	t.Helper()
	return filepath.Join(dir, strings.Repeat("x", 300)+".sock")
}

func setupLeftoverCannotBeChecked(t *testing.T, dir string, sys *system) string {
	t.Helper()
	path := filepath.Join(dir, "c.sock")
	staleSocket(t, path)
	sys.dial = func(context.Context, string) (net.Conn, error) { return nil, syscall.EAGAIN }
	return path
}

func setupLeftoverCannotBeRemoved(t *testing.T, dir string, sys *system) string {
	t.Helper()
	path := filepath.Join(dir, "c.sock")
	if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	sys.dial = func(context.Context, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
	return path
}

func setupModeCannotBeSet(t *testing.T, dir string, sys *system) string {
	t.Helper()
	sys.chmod = func(string, fs.FileMode) error { return errors.New("not permitted") }
	return filepath.Join(dir, "c.sock")
}

func TestClaimFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup claimSetup
		want  string
	}{
		{"directory cannot be created", setupDirectoryUnderAFile, "create socket directory"},
		{"directory cannot be examined", setupDirectoryCannotBeExamined, "check socket directory: io error"},
		{"directory is a symbolic link", setupDirectoryIsALink, "is not a directory"},
		{"directory is writable by others", setupDirectoryWritableByOthers, "can be written by other users"},
		{"path cannot be bound", setupPathTooLong, "listen on"},
		{"a leftover cannot be checked", setupLeftoverCannotBeChecked, "check for a running client"},
		{"a leftover cannot be removed", setupLeftoverCannotBeRemoved, "remove stale socket"},
		{"the mode cannot be set", setupModeCannotBeSet, "restrict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := shortDir(t)
			sys := systemHost()
			path := tt.setup(t, dir, &sys)
			sock, err := sys.claim(t.Context(), path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("claim = %v, want an error containing %q", err, tt.want)
			}
			if sock != nil {
				t.Error("claim returned a socket with an error")
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
