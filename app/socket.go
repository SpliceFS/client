package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrAlreadyRunning means another client already answers on the control socket of this
// configuration.
var ErrAlreadyRunning = errors.New("a client is already running for this configuration")

// ErrNotRunning means nothing answers on the control socket of this configuration.
var ErrNotRunning = errors.New("no client is running for this configuration")

// probeTimeout bounds the check whether a leftover socket still has a client behind it.
const probeTimeout = 2 * time.Second

// system is everything the control socket takes from the operating system, so tests can
// replace it.
type system struct {
	getenv  func(string) string
	geteuid func() int
	tempDir func() string
	abs     func(string) (string, error)
	lstat   func(string) (fs.FileInfo, error)
	chmod   func(string, fs.FileMode) error
	dial    func(ctx context.Context, path string) (net.Conn, error)
}

func systemHost() system {
	return system{
		getenv:  os.Getenv,
		geteuid: os.Geteuid,
		tempDir: os.TempDir,
		abs:     filepath.Abs,
		lstat:   os.Lstat,
		chmod:   os.Chmod,
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
}

// SocketPath returns the path of the control socket of the client that uses the
// configuration directory configDir: <dir>/splicefs/<16 hex digits>.sock, where <dir> is
// $XDG_RUNTIME_DIR, else /run for root, else the temporary directory, and the digits are
// the start of the SHA-256 of the absolute configuration directory. Two configurations
// therefore never share a socket, and every command that names the same directory finds
// the same one.
func SocketPath(configDir string) string {
	return systemHost().socketPath(configDir)
}

func (s system) socketPath(configDir string) string {
	abs, err := s.abs(configDir)
	if err != nil {
		abs = filepath.Clean(configDir)
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(s.runtimeDir(), "splicefs", hex.EncodeToString(sum[:8])+".sock")
}

func (s system) runtimeDir() string {
	if dir := s.getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	if s.geteuid() == 0 {
		return "/run"
	}
	return s.tempDir()
}

// absPath makes p absolute and clean. Mount targets are compared in this form, so that
// "mnt/files" typed in the directory that holds it names the configured target.
func (s system) absPath(p string) (string, error) {
	abs, err := s.abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

// socket is the listening control socket. Closing it removes the socket file.
type socket struct {
	*net.UnixListener
	once sync.Once
	err  error
}

// Close closes the socket and removes its file. It may be called more than once.
func (s *socket) Close() error {
	s.once.Do(func() { s.err = s.UnixListener.Close() })
	return s.err
}

// claim creates the control socket at path with mode 0600. A socket file left by a
// client that crashed is removed, but only after nothing answers on it: deleting the
// socket of a running client would leave it unreachable and let a second one start.
func (s system) claim(path string) (*socket, error) {
	if err := s.prepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	ln, err := listenUnix(path)
	if errors.Is(err, syscall.EADDRINUSE) {
		if err = s.clearStale(path); err != nil {
			return nil, err
		}
		ln, err = listenUnix(path)
	}
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	sock := &socket{UnixListener: ln}
	if err := s.chmod(path, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict %s: %w", path, err), sock.Close())
	}
	return sock, nil
}

func listenUnix(path string) (*net.UnixListener, error) {
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}

// clearStale removes the file at path if no client answers on it.
func (s system) clearStale(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, path)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("%w (socket %s)", ErrAlreadyRunning, path)
	case !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, fs.ErrNotExist):
		// A client that is too busy to accept is still a client. Only a refusal proves
		// that nobody is listening.
		return fmt.Errorf("check for a running client on %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

// prepareDir creates the directory of the socket and refuses one that somebody else
// could use to take the socket over. The temporary directory is shared, so a directory
// found there may have been created by another user.
func (s system) prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	info, err := s.lstat(dir)
	if err != nil {
		return fmt.Errorf("check socket directory: %w", err)
	}
	return checkDir(dir, info, s.geteuid())
}

func checkDir(dir string, info fs.FileInfo, uid int) error {
	switch {
	case !info.IsDir():
		return fmt.Errorf("socket directory %s is not a directory", dir)
	case !ownedBy(info, uid):
		return fmt.Errorf("socket directory %s belongs to another user", dir)
	case info.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("socket directory %s can be written by other users", dir)
	}
	return nil
}
