package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/SpliceFS/client/config"
)

// ErrAlreadyRunning means another client already answers on the control socket of this
// configuration.
var ErrAlreadyRunning = errors.New("a client is already running for this configuration")

// ErrNotRunning means nothing answers on the control socket of this configuration.
var ErrNotRunning = errors.New("no client is running for this configuration")

// ErrSocketDirNotDirectory means the place of the socket directory holds something else.
var ErrSocketDirNotDirectory = errors.New("is not a directory")

// ErrSocketDirForeign means the socket directory belongs to another user.
var ErrSocketDirForeign = errors.New("belongs to another user")

// ErrSocketDirWritable means other users can write in the socket directory, which would let
// them replace the socket.
var ErrSocketDirWritable = errors.New("can be written by other users")

const (
	// probeTimeout bounds the check whether a leftover socket still has a client behind it.
	probeTimeout = 2 * time.Second
	// privateFileMode lets only the owner read and write a file.
	privateFileMode = 0o600
	// privateDirMode lets only the owner use a directory.
	privateDirMode = 0o700
	// groupOtherWrite is the write permission of the group and of everybody else.
	groupOtherWrite = 0o022
)

// system is everything the control socket takes from the operating system, so tests can
// replace it.
type system struct {
	getenv  func(string) string
	geteuid func() int
	// configDir finds the default configuration directory.
	configDir func() (string, error)
	tempDir   func() string
	abs       func(string) (string, error)
	lstat     func(string) (fs.FileInfo, error)
	chmod     func(string, fs.FileMode) error
	dial      func(ctx context.Context, path string) (net.Conn, error)
}

func systemHost() system {
	return system{
		getenv:    os.Getenv,
		geteuid:   os.Geteuid,
		configDir: config.DefaultConfigDir,
		tempDir:   os.TempDir,
		abs:       filepath.Abs,
		lstat:     os.Lstat,
		chmod:     os.Chmod,
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
}

// SocketPath returns the path of the control socket of the client that uses the
// configuration directory configDir: <dir>/splicefs/<16 hex digits>.sock, where <dir> is
// $XDG_RUNTIME_DIR, else /run for root (/var/run on FreeBSD), else the temporary
// directory, and the digits are the start of the SHA-256 of the absolute configuration
// directory. Two configurations therefore never share a socket, and every command that
// names the same directory finds the same one.
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
		return rootRuntimeDir
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
func (s system) claim(ctx context.Context, path string) (*socket, error) {
	if err := s.prepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	listener, err := s.listen(ctx, path)
	if err != nil {
		return nil, err
	}
	sock := &socket{UnixListener: listener}
	if err := s.chmod(path, privateFileMode); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict %s: %w", path, err), sock.Close())
	}
	return sock, nil
}

// listen creates the socket at path. If a file is already there, it is removed when no
// client answers on it, and the socket is created again.
func (s system) listen(ctx context.Context, path string) (*net.UnixListener, error) {
	listener, err := listenUnix(path)
	if !errors.Is(err, syscall.EADDRINUSE) {
		return listener, err
	}
	if err := s.clearStale(ctx, path); err != nil {
		return nil, err
	}
	return listenUnix(path)
}

func listenUnix(path string) (*net.UnixListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	return listener, nil
}

// clearStale removes the file at path if no client answers on it.
func (s system) clearStale(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, path)
	switch {
	case err == nil:
		closeQuietly(conn)
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
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
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
		return fmt.Errorf("socket directory %s %w", dir, ErrSocketDirNotDirectory)
	case !ownedBy(info, uid):
		return fmt.Errorf("socket directory %s %w", dir, ErrSocketDirForeign)
	case info.Mode().Perm()&groupOtherWrite != 0:
		return fmt.Errorf("socket directory %s %w", dir, ErrSocketDirWritable)
	}
	return nil
}

// closeQuietly closes closer for a caller that has nothing left to lose when the close
// fails: the connection was only read, or everything on it was already sent, and it is
// dropped either way.
func closeQuietly(closer io.Closer) {
	_ = closer.Close() //nolint:errcheck // see above: nothing can be done about a failed close
}
