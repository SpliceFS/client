//go:build !linux && !freebsd

package config

// ordinaryUserFollowsXDG is false where the XDG Base Directory convention is not used: macOS
// has its own per-user folders (paths_darwin.go) and the other systems keep the folders of
// the installation.
const ordinaryUserFollowsXDG = false
