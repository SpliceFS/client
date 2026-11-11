// Package fuse is the Linux mount layer of the SpliceFS client. It serves a core.FS as a
// read-only FUSE filesystem and implements the contract of the mount package.
//
// The Mount entry point and everything that talks to the kernel or to go-fuse are built
// on Linux only. The logic that needs neither (error mapping, attribute conversion,
// the life cycle of a mount) has no build constraint so that it is also tested on other
// systems.
package fuse
