# SpliceFS

[![CI](https://github.com/BlueRival/splicefs/actions/workflows/ci.yml/badge.svg)](https://github.com/BlueRival/splicefs/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

SpliceFS mounts a read-only folder whose files are streamed from several servers at once.
Every server that holds a copy of a file serves blocks of it, and the client splices the
blocks back together in order, caching them on local disk.

It is built for throughput: a reader far away from every server gets the combined upload
speed of all of them, and a slow or failing server is routed around while a file is being read.

> **Status: pre-alpha.** The design is written down and the repository is set up. The server
> and the client are not implemented yet. Nothing here is usable today.

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

- **Server (`splicefsd`)** runs on each node and serves its local directories over HTTP:
  directory listings as JSON and file contents by byte range. It keeps no state about readers.
- **Client (`splicefs`)** asks every node what it has, merges the answers into one tree and
  mounts it with FUSE. When a file is read, it requests blocks from all nodes that hold an
  identical copy, gives more work to the faster nodes, and requests a late block again from
  another node.
- **Replication is not part of SpliceFS.** Use Syncthing, rsync, a clustered filesystem, or
  nothing at all: a cluster of one node works. SpliceFS only requires that a file with the
  same path, size and modification time has the same content on every node.

## Features (planned for 1.0)

- Read-only FUSE mount on Linux, running as an ordinary user
- One mount definition per target; a cluster root or any subdirectory can be mounted anywhere
- Several clusters in one client
- Multi-source block reads with per-node speed tracking and re-request of late blocks
- Local block cache with a size limit, kept across restarts
- Logins with per-user files and signed, short-lived tokens that any node can issue and check
- Works behind a reverse proxy or stands alone with its own TLS
- Plain HTTP API: every request can be reproduced with `curl`

## Documentation

- [Design](docs/design.md): architecture, API, consistency rules, block scheduling
- [Configuration](docs/configuration.md): server and client configuration reference
- [Contributing](CONTRIBUTING.md): development setup, standards, how changes are reviewed
- [Changelog](CHANGELOG.md)
- [Security policy](SECURITY.md)

## Requirements

Both programs are single static binaries with every library built in. There is nothing to
install for the server on any system, and nothing for the client in these cases:

- **Linux, client run as root** (for example from a systemd unit): nothing. The FUSE driver
  is part of every mainstream kernel.
- **macOS** (planned): nothing. The client uses the NFS client built into macOS.

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
make build      # binaries in ./bin: splicefsd (server), splicefs (client)
make check      # everything CI runs: format check, vet, tests with the race detector, coverage gate
```

## Repository layout

```
server/     the server program (splicefsd) and its packages
client/     the client program (splicefs) and its packages
internal/   code shared by both
docs/       design and configuration reference
scripts/    development helpers used by make and CI
```

## Versioning

SpliceFS follows [Semantic Versioning](https://semver.org). Releases are git tags of the form
`vMAJOR.MINOR.PATCH`. The server and the client are released together under one version, and
both print it with `--version`. Before 1.0.0 the API and the configuration format may change
in any minor release; every change is listed in the changelog.

## License

MIT. See [LICENSE](LICENSE).
