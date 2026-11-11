# SpliceFS client design

This document is the specification the code is written against. Where the code and this
document disagree, one of them has a bug. Sections marked *open* are not decided yet.

This document covers the client (`splicefs`). The goals, the terms (node, cluster, share,
block), the HTTP API and the consistency rules are in the
[design document of the server repository](https://github.com/SpliceFS/server/blob/main/docs/design.md),
which should be read first.

The client (`splicefs`) is one process that serves every configured mount.

## Structure and platforms

The client is one program built from two layers:

- **Core** (`core`): clusters, logins, the merged tree, block scheduling and the
  cache. It exposes a small read-only filesystem interface (look up, list, stat, open, read
  at an offset, filesystem statistics) and knows nothing about mounting. It is a library
  that other programs can import.
- **Mount layer** (`mount/...`): one implementation per way of mounting, selected at
  build time per operating system. Each one translates kernel requests into calls on the core.

| Platform | Mount layer | Extra software on the reader |
|---|---|---|
| Linux | FUSE | none as root; the `fuse3` package for an ordinary user |
| macOS | a local NFS server inside the client, mounted with the built-in NFS client | none |

FUSE on macOS needs third-party software (macFUSE or FUSE-T) whose kernel interface is in
transition, so the built-in NFS client is the dependable route there. The NFS server only
listens on the loopback address. See Known limitations for what that route costs.

## Discovery

Each cluster is configured with one entry URL. The client logs in there, fetches the node
list, and from then on talks to every node by its own URL. If the entry name resolves to
several addresses, they are tried in random order. A node is addressed by one URL; split
DNS decides whether that URL leads to an internal or a public address.

Login is sent to all nodes at once and the first success wins, so a user file that has not
reached every node yet still works.

## The merged tree

A directory listing is the union of the listings from all nodes. A node that answers `404`
contributes nothing; a node that does not answer is skipped and retried later, and never
makes entries disappear from a listing the client has already cached. Listings are cached
for a short, configurable time.

## Reading

- **Sequential reads** are served in blocks (default 5 MiB). The client keeps a read-ahead
  window in front of the read position and has several block requests in flight per node,
  over separate HTTP/1.1 connections.
- **Isolated reads**, such as a media scanner reading a header, fetch only the bytes asked
  for, rounded to a small granularity, so scanning a large library does not download a block
  per file. These reads are not cached.
- **Switching.** A read starts as isolated. Once reading has advanced sequentially by 1 MiB
  it switches to blocks, and the blocks that were first covered by small reads are fetched
  again as whole blocks behind the reader, so a file read from start to end is cached
  completely. A file of at most 1 MiB is always fetched whole.
- **Node choice.** The client measures each node's throughput and gives each node work in
  proportion to it.
- **Late blocks.** The client tracks how long blocks normally take per node. When the block
  the reader is waiting for is overdue, it is requested again from another node and the
  first complete answer is used. Slow nodes are given blocks further ahead in the window,
  where lateness costs nothing.
- **Failure.** A read that cannot be satisfied by any node within a time limit returns an
  I/O error. It does not block forever.

## Cache

Blocks are stored on local disk in one sparse file per remote file, with a map of the
blocks present. The cache is keyed by cluster, path and tag, so a file that changes gets a
new cache entry, and two mounts of the same cluster share cached blocks. It survives a restart.

Eviction removes the least recently read files first and is driven by three limits: a
maximum total size, a maximum age since a file was last read, and a minimum amount of free
space to leave on the cache disk. A file that is open is never evicted. A pass runs when the
client starts, whenever a write goes over a limit, and otherwise every ten minutes. With the
size and age limits set to `unlimited`, nothing is ever evicted and every file that is read
becomes a permanent local copy. When the cache disk is full, reads still succeed; the blocks
are passed through without being stored.

Files smaller than a configurable minimum size are never cached; they are read through
from the nodes each time.

**Revalidation.** A cache entry records when its file was last checked against the nodes.
Once that is older than the configured revalidation time, the next open asks the nodes
again and compares name, size, modification time and head digest. If anything differs, the
entry is discarded and the file is read fresh. If no node can be reached, a file that is
completely cached is still served from the cache, and the check is made at the next open
after a node returns. Each mount can turn this off, in which case such reads fail.

The cache directory must not be a mount target, lie inside one, or contain one. The client
refuses to start otherwise.

## Free space

A mount reports, per share, the smallest total and the smallest free space of any node
that answers. Every node is expected to end up holding every file, so the fullest node is
the limit. A mount of the cluster root adds up its shares.

## Filesystem behavior

The mount has to behave like a local disk for ordinary tools. These are requirements:

- **Read-only.** Any attempt to write, create, rename, remove, or change attributes or
  times fails with `EROFS`. Reading a file does not update its access time, on the mount or
  on the nodes.
- **Stable identity.** A path keeps the same inode number for the life of the mount and
  across restarts, as long as it stays the same kind of thing (file or directory). Files
  report one link, mode `0444`; directories mode `0555`; owner and group are the user running
  the client. `st_blocks` reflects the file size.
- **Times.** Modification time comes from the nodes. Access and change time equal it.
- **Reads arrive out of order.** The kernel issues read-ahead requests concurrently and not
  in sequence; the detection of sequential reading tolerates that.
- **Interrupted reads** cancel their block requests.
- **Names that do not exist are remembered** for a short time, and the names desktop
  systems probe constantly (`.DS_Store`, `._*`, `.Spotlight-V100`, `.hidden`, `.Trash*`) are
  refused without asking any node.
- **Extended attributes** are not supported; requests are refused in a way that stops the
  kernel from asking again.
- **Locks** are local. They always succeed and are not shared with other readers, which is
  enough because the files cannot change; refusing them would break programs that lock
  before reading.
- **Names.** Names are passed through byte for byte. A name that is a file on one node and
  a directory on another is listed as the kind most nodes report. Accented names behave as
  a downloaded file would on the same system: Linux matches names byte for byte; macOS keeps
  the name as stored and also finds it when a program asks in the other Unicode form.
- **A file that changes while open** keeps serving the version that was opened, as long as
  one node still has it; a new open sees the new version.
- **All nodes unreachable** is an I/O error, never "not found" and never an empty directory.
- **Start and stop.** A stale mount left by a crash is detected and cleared at start. The
  client unmounts on `SIGINT` and `SIGTERM`, and tells systemd when all mounts are ready.
- **Kernel caching.** File contents never change under a given tag, so the kernel may keep
  file data and directory entries cached.

## Known limitations

- **Offline use needs a running client.** Directory listings are kept in memory only. A
  client that is restarted while no node can be reached has nothing to list, even though
  file contents are still in its cache.
- **Linux: read-ahead cannot be interrupted.** The kernel does not forward the interruption
  of a program to requests it issued as read-ahead, so a killed reader's last block requests
  run to completion.
- **macOS: an outage can look like a missing file.** The NFS server library answers a failed
  name lookup as "not found" whatever the cause. A name looked up for the first time while
  no node can be reached is reported missing, and macOS may remember that for up to 30
  seconds. Names that were already known keep working and report I/O errors correctly.
- **macOS: a read that waits longer than about 9 seconds fails.** The mount is "soft" so
  that a crashed client gives errors and not a frozen desktop, and macOS does not let that
  timeout be raised. A block that cannot be fetched from any node in that time fails the read.
- **macOS: requests are served one at a time per connection,** which caps throughput below
  what the Linux mount reaches.

The limitations of the server are listed in its
[design document](https://github.com/SpliceFS/server/blob/main/docs/design.md#known-limitations).

## Open questions

- Per-block checksums (see
  [Consistency](https://github.com/SpliceFS/server/blob/main/docs/design.md#consistency)
  in the server design document).
- Keeping directory listings on disk for offline starts.
- A mount helper for `/etc/fstab`.
