# SpliceFS client design

This document is the specification the code is written against. Where the code and this
document disagree, one of them has a bug. Sections marked *open* are not decided yet.

This document covers the client (`splicefs`). The goals, the terms (node, cluster, share,
block), the HTTP API and the consistency rules are in the
[design document of the server repository](https://github.com/SpliceFS/server/blob/main/docs/design.md),
which should be read first.

The client (`splicefs`) is one process that serves every configured mount. It keeps running
when it has none.

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
| FreeBSD | FUSE | the `fusefs` kernel module, loaded |
| macOS | a local NFS server inside the client, mounted with the built-in NFS client | none |

FUSE on macOS needs third-party software (macFUSE or FUSE-T) whose kernel interface is in
transition, so the built-in NFS client is the dependable route there. The NFS server only
listens on the loopback address. See Known limitations for what that route costs.

## Configuration

The configuration is a folder of small files, so that a package can ship the folder and an
administrator can add files without editing one that a package owns. The reference for every
key is the [configuration document](configuration.md).

- `conf.d/*.yaml` holds settings. They are read in name order and merged, a later file winning.
  Today the settings are the cache keys that every mount inherits.
- `clusters.d/<name>.yaml` is one cluster. The file name is the cluster name.
- `mounts.d/<name>.yaml` is one mount. The file name is the mount name, and mounts are ordered
  by it. A mount may override any cache key.
- Relative paths are relative to the configuration folder.
- An empty or missing folder is a valid configuration with no clusters and no mounts. A fresh
  install runs in that state.
- The whole configuration is validated in one pass. Every problem is reported, and each names
  the file its value came from.

## Lifetime and reload

The run ends only when the client is told to stop. A mount that ends does not end the run. It
may end because it was unmounted through the control socket, because the administrator ran
`umount`, or because a reload detached it. The client takes it out of its set and gives back
what the mount used. A mount that ended without a reload stays unmounted until the next reload
or restart. With no mounts the client idles, and it tells systemd that it is ready.

`SIGHUP` applies the configuration without a restart. Reloads run one at a time.

1. The client reads and validates the whole configuration. It then makes the clusters and
   cache folders match it. Anything that can fail, such as an unreadable password file, a
   cache folder that cannot be opened, or two mounts that share a cache folder with different
   limits, is done before anything changes. If it fails, nothing changes and the error is
   logged.
2. A mount that is no longer configured, whose settings changed, or whose cluster or cache
   folder was replaced is detached. The detach is forced: the mount point disappears at once,
   and files that are open keep working until they are closed.
3. Every configured mount that is not live is mounted. This covers new mounts and the ones
   that were just detached because they changed. The cluster is logged in to first, as at
   start. An unreachable cluster is a warning and the mount is made anyway. Rejected
   credentials, or a mount that cannot be made, are logged and that mount is skipped. The
   others are still applied.
4. A mount with nothing changed is not touched.

Mounts share clusters and cache folders, and a reload changes them while mounts use them:

- A cluster whose settings, password and listing TTL are unchanged is kept, with its session
  and its measured node speeds. Any other cluster is built new.
- A cache folder that is already open is kept, with its files, and the next eviction pass uses
  the new limits. A folder that is new is opened.
- A cluster or cache folder that a reload replaced or removed is retired. It goes on serving
  the mounts that already use it, and it is closed when the last of them has ended, or at
  once if none uses it.
- Each cluster has its own HTTP connection pool. It is closed with the cluster, so replacing
  one cluster never drops the connections of another.

Stopping unmounts every mount that is live, with force if needed, and then closes everything,
including what detached mounts still held.

### Requested mounts

On Linux, `mount -t splicefs` and fstab lines of type `splicefs` ask the running client for a
mount. The mount helper is the `splicefs` program started under the name `mount.splicefs`. It
only sends one request on the control socket. The client makes the mount.

- All mount changes happen in the goroutine of the run loop: reloads, requests from the
  control socket, and the restore at start. The control socket hands a request to the run loop
  and waits for the answer, so changes never overlap.
- A requested mount is part of the effective configuration, which is the files plus the
  requested mounts. It is checked by the same rules as the files, so it cannot overlap a mount
  file or another requested mount, and its cluster must be defined. A request that breaks a
  rule is refused and changes nothing.
- A reload reads the files and adds the requested mounts that are still mounted. If the result
  is invalid only because of a requested mount, that mount is dropped with a warning and the
  rest is applied.
- The list is a file next to the control socket, in the runtime directory, with the extension
  `.mounts.json`. It is gone after a reboot, when fstab asks again. A mount that ends because
  of `umount` leaves the list. A mount that ends because the client stops stays on it, so a
  restart of the service brings it back.

### Health check and heartbeat

The run loop checks every live mount every `probeInterval` (10 seconds) and then tells the
service manager that it is alive. Both happen in the goroutine of the run loop, so a check
never overlaps a reload or a mount request.

- **The probe name.** `mount.ProbeName` is `.splicefs-probe`. Directly in the root of every
  mount it never exists. Both mount layers answer a lookup of it with "not found" at once,
  before any call into `core.FS`. No network, cache or lock that a slow read could hold is
  involved, so the answer proves only that a request travels from the kernel to the client
  and back. It is never listed. In a subdirectory it is an ordinary name.
- **The check.** The loop calls `os.Lstat` on `<target>/.splicefs-probe` in a goroutine of its
  own and waits for `probeTimeout` (5 seconds). "Does not exist" passes. Any other answer, or
  no answer in time, fails. A check that is still waiting from an earlier round fails the
  mount without a second one being started, so a hung mount holds one goroutine and not one
  per round. A passing check resets the count of the mount.
- **A hung mount.** After `probeFailures` (3) failed checks in a row the client logs
  `mount does not answer, mounting it again` as an error, force unmounts the mount and takes
  it out of its set. It then mounts what is missing, the same step a reload ends with, from
  the configuration applied last and the requested mounts. The files are not read again. A
  requested mount stays on the requested list.
- **Kernel connection (Linux).** After the forced unmount of a hung mount the run loop
  aborts the kernel connection of that mount. This is its own operation (`mount.Aborter`),
  never part of an unmount: a reload detaches mounts with a forced unmount too, and their
  open files must keep working. The connection number is read from `/proc/self/mountinfo` when
  the mount is made and kept, because after the detach the mount point no longer leads to
  the mount. The mount table is read and the mount point is not looked at with `stat`,
  because a `stat` fills the attribute cache and a dead mount would then answer from it.
  Requests that wait on a dead connection fail and do not wait forever.
- **macOS.** The root attributes and the capacity are answered without the cluster: the
  attributes of the root wait at most one second for the core and otherwise give the last
  answer, or a fixed read-only directory before the first one. The capacity gives the last
  value the core reported, or zero. The kernel asks for both while it mounts, so the mount is
  made while no server can be reached. Listings fail with an I/O error until a server
  answers, and then the mount works without a restart.
- **The heartbeat.** After each round, whatever it found, the client sends `WATCHDOG=1`. It
  proves that the run loop is alive, not that every mount is healthy. The systemd unit has
  `WatchdogSec=300`, so a client whose run loop is stuck for five minutes is killed and
  started again by `Restart=`. The OpenRC service has no heartbeat: `supervise-daemon`
  restarts the client only when it exits.
- The ticker, the lstat function, the timeout and the failure count can be replaced in tests.

## Discovery

Each cluster is configured with one URL, the cluster host: one name for the whole cluster,
which resolves to as many of its servers as possible. Every server also has its own node host,
which it publishes. The client logs in at the cluster host, including every time it renews its
token, and asks the cluster host for the list of node hosts again every minute, because the
call is cheap. Only requests for file contents, directory listings and statistics go to the
node hosts. A node is addressed by one URL; split DNS decides whether that URL leads to an
internal or a public address. If the cluster lists no nodes, as a single server does until its
public address is set, the cluster host is the only node. Its name is the host and port of the
URL. A later list with nodes replaces it.

The client needs the cluster host to log in and to learn the node hosts. If the cluster host
cannot be reached, the client assumes that no node can be reached either and treats the
cluster as unreachable for those two things: it does not log in or ask for the list at a node
host. It keeps using the node hosts it already knows until its login runs out, and it recovers
by itself when the cluster host answers again.

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
again and compares name, size, modification time, head digest and tail digest. If anything
differs, the entry is discarded and the file is read fresh. If no node can be reached, a file
that is completely cached is still served from the cache, and the check is made at the next open
after a node returns. Each mount can turn this off, in which case such reads fail.

The cache directory must not be a mount target, lie inside one, or contain one. The client
refuses to start, and a reload refuses to apply the configuration, otherwise.

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
- **Start and stop.** The client unmounts on `SIGINT` and `SIGTERM`, and tells systemd when
  all mounts are ready, which with no mounts is at once. See Lifetime and reload. A mount
  left behind by a client that crashed or was killed is handled per system:
  - **Linux and FreeBSD:** the mount is detected and cleared when the client starts.
  - **macOS:** nothing runs at login or at start, so nothing clears it by itself. The
    client writes a record of the NFS mounts it makes, in the runtime directory next to the
    control socket (the same name with the ending `.nfs.json`). `splicefs umount`, when no
    client is running, finds a recorded mount that is still in the mount table with nothing
    answering on its port, and removes it with `umount -f`. A mount whose port still answers
    is left alone. `splicefs mount` refuses to mount on a target that such a mount still
    holds, and says to run `splicefs umount TARGET`; it does not clear the mount itself. The
    record is gone after a reboot, which also removes the mounts it lists.
- **Kernel caching.** File contents never change under a given tag, so the kernel may keep
  file data and directory entries cached.

## Known limitations

- **Offline use needs a running client.** Directory listings are kept in memory only. A
  client that is restarted while no node can be reached has nothing to list, even though
  file contents are still in its cache.
- **FreeBSD: a hung mount cannot be aborted.** On Linux the client cuts the kernel connection
  of a hung mount after the forced unmount, so the requests that wait on it fail. FreeBSD has
  no interface for that, so the client cannot free programs that already wait on a hung
  mount. It still unmounts the mount and mounts it again.
- **FreeBSD: a missing name is asked for every time.** Linux remembers for a short time that a
  name does not exist. FreeBSD's FUSE does not, so every look-up of a missing name goes to the
  client again.
- **FreeBSD: a forced unmount also ends open files.** FreeBSD has no lazy unmount, so the
  client detaches a mount with a forced unmount (`MNT_FORCE`), which makes files that are
  still open fail. On Linux a detached mount keeps serving its open files. A mount that is
  in use is refused with "busy" when the client unmounts it without force.
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
