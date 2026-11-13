//go:build freebsd

package app

// rootRuntimeDir is where root keeps runtime files. FreeBSD has no /run.
const rootRuntimeDir = "/var/run"
