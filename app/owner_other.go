//go:build !unix

package app

import "io/fs"

// ownedBy cannot tell on this system and trusts the directory.
func ownedBy(fs.FileInfo, int) bool { return true }
