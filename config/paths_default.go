//go:build !darwin

package config

// Directories of a system-wide installation, which a package manager manages.
const (
	osConfigDir = "/etc/splicefs/client"
	osCacheDir  = "/var/cache/splicefs"
)
