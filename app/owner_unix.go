//go:build unix

package app

import (
	"io/fs"
	"syscall"
)

// ownedBy reports whether the file belongs to the user with the given id.
func ownedBy(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
