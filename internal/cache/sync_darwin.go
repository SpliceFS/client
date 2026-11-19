package cache

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// orderedSync makes the file's writes reach the disk before any later write. The cache
// needs only that order (data before the metadata that names it), and on macOS File.Sync
// is F_FULLFSYNC, a flush through the drive's own cache that costs several milliseconds.
// F_BARRIERFSYNC gives the order at a fraction of that.
func orderedSync(file *os.File) error { return barrierSync(file, fcntlBarrier) }

// barrierSync falls back to the full sync when the file system does not support barriers.
func barrierSync(file *os.File, barrier func(fd uintptr) error) error {
	if barrier(file.Fd()) == nil {
		return nil
	}
	//nolint:wrapcheck // the callers say which file and why; the os error names the path
	return file.Sync()
}

func fcntlBarrier(fd uintptr) error {
	if _, err := unix.FcntlInt(fd, unix.F_BARRIERFSYNC, 0); err != nil {
		return fmt.Errorf("barrier sync: %w", err)
	}
	return nil
}
