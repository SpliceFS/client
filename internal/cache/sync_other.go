//go:build !darwin

package cache

import "os"

// orderedSync makes the file's writes reach the disk before any later write. On these
// systems File.Sync is fsync, which is what the order needs and no more.
func orderedSync(file *os.File) error {
	//nolint:wrapcheck // the callers say which file and why; the os error names the path
	return file.Sync()
}
