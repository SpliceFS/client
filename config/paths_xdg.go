//go:build linux || freebsd

package config

// ordinaryUserFollowsXDG is true where desktop and user tools follow the XDG Base Directory
// convention, so that a program run by an ordinary user keeps its files in that user's home
// directory and not in the folders of a system-wide installation.
const ordinaryUserFollowsXDG = true
