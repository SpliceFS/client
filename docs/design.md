# SpliceFS design

This document is the specification the code is written against. Where the code and this
document disagree, one of them has a bug. Sections marked *open* are not decided yet.

## Goals

1. A reader mounts a read-only folder and gets each file from every node that holds it,
   at the combined speed of those nodes.
2. A slow or unreachable node costs throughput, never correctness and never a hung read.
3. The server side is simple enough to audit in an afternoon: stateless, plain HTTP, no
   database.
4. Nothing depends on a particular replication tool, DNS setup, proxy or number of nodes.
   One node is a valid cluster.

## Non-goals

- Writing through the mount.
- Replicating data between nodes.
- Low first-byte latency. The design favors sustained throughput.

## Terms

| Term | Meaning |
|---|---|
| node | one machine running the server, with the files on local disk |
| cluster | the set of nodes listed in one node list |
| share | a named directory a node offers |
| block | a fixed-size byte range of a file, the unit the client requests and caches |

## Server

The server (`splicefsd`) is an HTTP server. It reads files; it never writes to a share.

### API, version 1

All paths are below a configurable base path (default `/api`). Requests other than health
and login need `Authorization: Bearer <token>`.

| Method and path | Answer |
|---|---|
| `GET /v1/health` | `200` and `{"status":"ok","version":"..."}`. No login. For load balancers and DNS health checks. |
| `POST /v1/login` | Body `{"user":"...","password":"..."}`. `200` and `{"token":"...","expires":"<RFC 3339>"}`, or `401`. |
| `GET /v1/nodes` | `{"cluster":"...","nodes":[{"name":"...","url":"..."}]}` from the node list file. |
| `GET /v1/tree/` | The shares this user may read, as directory entries. |
| `GET /v1/tree/<share>/<dir>/` | `{"entries":[{"name":"...","dir":false,"size":123,"mtime":"<RFC 3339>","etag":"..."}]}`, sorted by name. `404` if the directory does not exist on this node. |
| `GET`, `HEAD /v1/file/<share>/<path>` | The file. Supports `Range` (one range), `If-Range` and `If-Match`. Sends `ETag` and `Accept-Ranges: bytes`. |

Errors are JSON, `{"error":"..."}`, with the usual status codes: `400` malformed request,
`401` missing or invalid token, `403` share not allowed for this user, `404` not found,
`412` and `416` for failed preconditions and bad ranges, `429` login rate limit.

### What a node serves

- Regular files and directories inside a share. Requests are resolved inside the share's
  root; a path that leaves it, by `..` or by a symbolic link, is `404`.
- Entries matching a share's `exclude` patterns are neither listed nor served. The default
  excludes names starting with a dot, which covers the temporary files of most replication
  tools.
- Files modified more recently than the share's `min_age` are treated as absent. This
  protects against replication tools that write files in place. The default is zero, which
  is right for tools that write to a temporary name and rename.

### Entity tags

A file's tag is derived from its size and its modification time, truncated to the
configured precision (default one second). Two nodes therefore produce the same tag for the
same file whenever the replication tool preserves modification times. The tag is the only
thing the client uses to decide that two copies are the same file.

### Compression

A response is compressed only when the client asks for it, the request has no `Range`, and
the content type is compressible. Block requests never ask for compression, so they always
receive exact byte ranges.

### Logins

- One file per user in the users directory: a password hash (argon2id, PHC string format),
  the shares the user may read, and an optional `disabled` flag.
- A token is a compact JWT signed with HMAC-SHA256. It carries the user name, the allowed
  shares and an expiry. Any node holding the signing key can issue and check tokens, with no
  communication between nodes.
- The keys file lists one or more keys. The first signs; all verify. Rotating a key means
  adding a new first key, waiting one token lifetime, and removing the old one.
- Tokens are short-lived (default 30 minutes). Removing or disabling a user stops new
  tokens at once; tokens already issued run out on their own.
- Login attempts are rate limited per client address. Behind a reverse proxy the address
  comes from `X-Forwarded-For`, and only when the request arrives from a configured trusted
  proxy.

The users directory, the keys file and the node list are ordinary files. How they reach
every node is the operator's choice.

## Consistency

SpliceFS assumes: *a file with the same path, size and modification time has the same
content on every node.* It enforces what it can:

1. When a file is opened, the client groups the nodes by the tag they report and reads only
   from the largest group. Ties go to the newest modification time.
2. Every block request carries `If-Match` with that tag. A node whose copy has changed
   answers `412`, and the client drops it for that file.
3. A file that disappears from every node during a read fails the read with an I/O error.

Content hashes are not part of version 1. *Open:* an optional per-block checksum for
deployments that cannot trust modification times.

## Client

The client (`splicefs`) is one process that serves every configured mount.

### Discovery

Each cluster is configured with one entry URL. The client logs in there, fetches the node
list, and from then on talks to every node by its own URL. If the entry name resolves to
several addresses, they are tried in random order. A node is addressed by one URL; split
DNS decides whether that URL leads to an internal or a public address.

Login is sent to all nodes at once and the first success wins, so a user file that has not
reached every node yet still works.

### The merged tree

A directory listing is the union of the listings from all nodes. A node that answers `404`
contributes nothing; a node that does not answer is skipped and retried later, and never
makes entries disappear from a listing the client has already cached. Listings are cached
for a short, configurable time.

### Reading

- **Sequential reads** are served in blocks (default 5 MiB). The client keeps a read-ahead
  window in front of the read position and has several block requests in flight per node,
  over separate HTTP/1.1 connections.
- **Isolated reads**, such as a media scanner reading a header, fetch only the bytes asked
  for, rounded to a small granularity, so scanning a large library does not download a block
  per file.
- **Node choice.** The client measures each node's throughput and gives each node work in
  proportion to it.
- **Late blocks.** The client tracks how long blocks normally take per node. When the block
  the reader is waiting for is overdue, it is requested again from another node and the
  first complete answer is used. Slow nodes are given blocks further ahead in the window,
  where lateness costs nothing.
- **Failure.** A read that cannot be satisfied by any node within a time limit returns an
  I/O error. It does not block forever.

### Cache

Blocks are stored on local disk in one sparse file per remote file, with a map of the
blocks present. The cache is keyed by cluster, path and tag, so a file that changes gets a
new cache entry. The cache has a size limit, evicts least recently used files first, and
survives a restart.

### Mount

The mount is read-only FUSE on Linux. File contents never change under a given tag, so the
kernel is allowed to keep file data and directory entries cached.

## Deployment shapes

- **Behind a reverse proxy.** The proxy terminates TLS and forwards the base path to the
  server over plain HTTP on a private address.
- **Standalone.** The server terminates TLS itself with a certificate and key from its
  configuration.

## Open questions

- Per-block checksums (see Consistency).
- Revoking one token before it expires.
- A mount helper for `/etc/fstab`.
