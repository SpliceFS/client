# SpliceFS client

[![CI](https://github.com/SpliceFS/client/actions/workflows/ci.yml/badge.svg)](https://github.com/SpliceFS/client/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

SpliceFS mounts a read-only folder whose files are streamed from several servers at once.
Every server that holds a copy of a file serves blocks of it, and the client splices the
blocks back together in order, caching them on local disk.

It is built for throughput: a reader far away from every server gets the combined upload
speed of all of them, and a slow or failing server is routed around while a file is being read.

This repository holds the client, `splicefs`, which mounts the files. The server that runs on
each node, `splicefsd`, is in the [server repository](https://github.com/SpliceFS/server).

> **Status: alpha.** The server and the client work end to end on Linux and macOS and are
> covered by tests, but nothing has been run in production yet and no release has been
> tagged. The API and the configuration format may still change. Read the
> [known limitations](docs/design.md#known-limitations) first.

## How it works

```
                +------------------+
                |  node: east      |  files on local disk
            +-->|  splicefsd       |
            |   +------------------+
+--------+  |   +------------------+
| reader |--+-->|  node: west      |  same files, kept in step by any
| mount  |  |   |  splicefsd       |  replication tool you already use
+--------+  |   +------------------+
            |   +------------------+
            +-->|  node: ...       |
                |  splicefsd       |
                +------------------+
```

- **Server (`splicefsd`)**, which is in its [own repository](https://github.com/SpliceFS/server),
  runs on each node and serves its local directories over HTTP: directory listings as JSON and
  file contents by byte range. It keeps no state about readers.
- **Client (`splicefs`)** asks every node what it has, merges the answers into one tree and
  mounts it with FUSE. When a file is read, it requests blocks from all nodes that hold an
  identical copy, gives more work to the faster nodes, and requests a late block again from
  another node.
- **Replication is not part of SpliceFS.** Use Syncthing, rsync, a clustered filesystem, or
  nothing at all: a cluster of one node works. SpliceFS only requires that a file with the
  same path, size and modification time has the same content on every node.

## Features

- Read-only mount on Linux (FUSE) and macOS (through the built-in NFS client)
- One mount definition per target; a cluster root or any subdirectory can be mounted anywhere
- Several clusters and several mounts in one client
- Multi-source block reads with per-node speed tracking and re-request of late blocks
- Local block cache with size, age and free-space limits, kept across restarts; files that
  were read completely stay readable when every node is unreachable

## Quick start

The nodes must be running first. See the server's
[quick start](https://github.com/SpliceFS/server#quick-start).

On the reader:

```
# /etc/splicefs/client/config.yaml
cache: {dir: /var/cache/splicefs, max_size: 200GiB}
clusters:
  example: {url: "https://east.example.com/api", user: alice, password_file: secrets/example}
mounts:
  - {cluster: example, path: /, target: /mnt/files}
```

```
splicefs mount          # stays in the foreground; Ctrl-C unmounts
splicefs status         # from another terminal: mounts, node speeds, cache use
splicefs umount
```

The [configuration reference](docs/configuration.md) lists every setting.

## Documentation

- [Design](docs/design.md): structure, discovery, the merged tree, block scheduling, cache
- [Configuration](docs/configuration.md): client configuration reference and commands
- [Contributing](CONTRIBUTING.md): development setup, standards, how changes are reviewed
- [Changelog](CHANGELOG.md)
- [Security policy](SECURITY.md)
- [Server](https://github.com/SpliceFS/server): the server, the HTTP API and the consistency rules

## Requirements

`splicefs` is a single static binary with every library built in. There is nothing to install
in these cases:

- **Linux, client run as root** (for example from a systemd unit): nothing. The FUSE driver
  is part of every mainstream kernel.
- **macOS**: nothing. The client uses the NFS client built into macOS.

One case needs a package:

- **Linux, client run as an ordinary user.** Mounting without root goes through the
  `fusermount3` helper from the FUSE package.

  | System | Command |
  |---|---|
  | Debian, Ubuntu | `sudo apt install fuse3` |
  | Fedora, RHEL, Rocky, Alma | `sudo dnf install fuse3` |
  | openSUSE | `sudo zypper install fuse3` |
  | Arch | `sudo pacman -S fuse3` |
  | Alpine | `sudo apk add fuse3` |

  To let other users read a mount (`allow_other`), also uncomment `user_allow_other` in
  `/etc/fuse.conf`.

## Building

Requires Go 1.26 or newer.

```
make build      # the binary is ./bin/splicefs
make check      # everything CI runs: format check, vet, tests with the race detector, coverage gate
make check-linux  # the same on Linux with real FUSE mounts, in a container (needs Docker)
make lint         # golangci-lint
```

## Repository layout

```
cmd/splicefs/  the splicefs program: the mount, umount and status commands
app/           builds the client from a configuration, mounts it, serves the control socket
config/        loads and validates config.yaml
core/          the client as an importable library (github.com/SpliceFS/client/core)
mount/         the contract every mount layer offers; mount/fuse is Linux, mount/nfs is macOS
internal/      node HTTP client, cluster session, merged tree, block cache, block reader, and
               units, version and wire (the JSON types of the HTTP API), which also exist in
               the server repository
docs/          design and configuration reference
packaging/     systemd unit
scripts/       development helpers used by make and CI
```

## Versioning

SpliceFS follows [Semantic Versioning](https://semver.org). Releases are git tags of the form
`vMAJOR.MINOR.PATCH` in this repository, and `splicefs --version` prints the version. The
server and the client are versioned separately: each repository has its own version tags. A
client works with a server that serves the API version the client speaks, and that version is
part of the URL (`/v1/`). Before 1.0.0 the API and the configuration format may change in any
minor release; every change is listed in the changelog.

## License

MIT. See [LICENSE](LICENSE).
