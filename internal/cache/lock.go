package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockName is the file at the top of a cache folder that carries the lock. It is never
// removed: unlinking a lock file lets a second process lock the new file while the first
// still holds the old one.
const lockName = "lock"

// ErrInUse means another process holds the cache folder. Two processes working on one
// folder would truncate each other's data files, so the second one is refused.
var ErrInUse = errors.New("another splicefs process is using the cache folder")

// flocker is syscall.Flock. Tests replace it to reach the failures that a real lock call
// does not produce.
type flocker func(fd, how int) error

// lockFolder takes the exclusive lock of the cache folder dir and returns the open lock
// file. The lock belongs to that open file and lasts until it is closed or the process
// ends, so a crash or a reboot never leaves a stale lock behind and nothing has to clean
// one up. The call never waits: a folder that is taken is an error at once.
func lockFolder(dir string, flock flocker) (*os.File, error) {
	name := filepath.Join(dir, lockName)
	file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	// The conversion cannot wrap: a file descriptor is a small non-negative number.
	if err := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		err = fmt.Errorf("lock %s: %w", name, err)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = fmt.Errorf("%w: %s", ErrInUse, dir)
		}
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
