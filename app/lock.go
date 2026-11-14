package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockExt replaces the extension of the control socket to name its lock file.
const lockExt = ".lock"

// lockPath returns the lock file of the control socket at socketPath: next to the socket,
// in the runtime directory, so that every client of one configuration finds the same one.
func lockPath(socketPath string) string {
	return socketPath[:len(socketPath)-len(filepath.Ext(socketPath))] + lockExt
}

// lockSocket takes the exclusive lock of the control socket at socketPath and returns the
// open lock file. A client holds it for as long as it runs, and only the holder may judge
// a socket file stale and remove it: without the lock, two clients that found the same
// leftover could each remove the socket the other had just created, and both would run.
// The lock belongs to that open file and lasts until it is closed or the process ends, so
// a crash never leaves a stale lock behind. The file is never removed: unlinking it lets a
// second client lock the new file while the first still holds the old one. The call never
// waits: a socket that is taken is an error at once.
func (s system) lockSocket(socketPath string) (*os.File, error) {
	name := lockPath(socketPath)
	file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	// The conversion cannot wrap: a file descriptor is a small non-negative number.
	if err := s.flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = fmt.Errorf("%w (socket %s)", ErrAlreadyRunning, socketPath)
		} else {
			err = fmt.Errorf("lock %s: %w", name, err)
		}
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
