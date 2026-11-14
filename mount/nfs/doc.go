// Package nfs is the macOS mount layer of the SpliceFS client. It serves a core.FS
// read-only over NFSv3 on a loopback port and mounts that server with the NFS client
// that is built into macOS, so nothing has to be installed on the Mac.
//
// The package has three parts that do not know about each other:
//
//   - a billy.Filesystem over core.FS (the interface go-nfs serves),
//   - a core.FS decorator that finds names asked for in another Unicode normalization
//     form than the one stored,
//   - the loopback NFS server itself.
//
// Only the mounting step is specific to macOS: Mount is defined on darwin. The rest
// builds everywhere, which lets the protocol be tested on any operating system.
//
// # Behavior that differs from the FUSE layer
//
// go-nfs is stateless and opens a file for every READ request, so the billy layer sees
// one Open and one Stat per read request. The core.FS sees fewer opens: the layer keeps
// the opened core.File of each path for as long as READs keep coming and closes it after
// two seconds without one, or when the mount ends. The READs of a file that is being
// read, also those that arrive together, so read the one version that was chosen at the
// first open, and a file that is replaced is seen by the first READ after the quiet time,
// like a new open. A read that fails drops the kept file, so the next READ opens again.
//
// go-nfs serves the requests of one connection one after another. Its status mapping is
// fixed: a failed LOOKUP is always reported as "no such file", a failed READDIR as "not a
// directory" and a failed open for READ as "permission denied", whatever the cause. Only
// GETATTR and the read itself report an I/O error for core.ErrUnavailable.
//
// The macOS mount is soft, so that a program does not hang when the client is gone. The
// kernel then gives up on a request that the server has not answered after about nine
// seconds (measured; the timeo and retrans options did not change it) and the program
// gets ETIMEDOUT, although the server may still be working on the call.
package nfs
