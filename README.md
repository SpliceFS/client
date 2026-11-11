# SpliceFS

[![CI](https://github.com/BlueRival/splicefs/actions/workflows/ci.yml/badge.svg)](https://github.com/BlueRival/splicefs/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

SpliceFS mounts a read-only folder whose files are streamed from several servers at once.
Every server that holds a copy of a file serves blocks of it, and the client splices the
blocks back together in order, caching them on local disk.

It is built for throughput: a reader far away from every server gets the combined upload
speed of all of them, and a slow or failing server is routed around while a file is being read.

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

- **Server (`splicefsd`)** runs on each node and serves its local directories over HTTP:
  directory listings as JSON and file contents by byte range. It keeps no state about readers.
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
- Logins with per-user files and signed, short-lived tokens that any node can issue and check
- Works behind a reverse proxy or stands alone with its own TLS
- Plain HTTP API: every request can be reproduced with `curl`

## Quick start

On every node, with the files already in place under `/srv/media`:

```
# /etc/splicefs/server/config.yaml
listen:
  - {address: 0.0.0.0, port: 8480}
shares:
  media: {path: /srv/media}
cluster: {name: example, node: east, nodes_file: /srv/shared/nodes.yaml}
auth: {users_dir: /srv/shared/users, keys_file: /srv/shared/keys.yaml}
```

The node list, the users directory and the keys file are the same on every node; copy or
replicate them however you like. Create them once:

```
# /srv/shared/nodes.yaml
cluster: example
nodes:
  - {name: east, url: "https://east.example.com/api"}
  - {name: west, url: "https://west.example.com/api"}

# /srv/shared/keys.yaml  (mode 600)
keys:
  - {id: "2026-11", secret: "<output of: head -c 48 /dev/urandom | base64>"}

# /srv/shared/users/alice.yaml
password_hash: "<output of: splicefsd hash-password>"
shares: [media]
```

Start `splicefsd` on each node (a systemd unit is in `packaging/systemd`). On the reader:

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
make build      # binaries in ./bin: splicefsd (server), splicefs (client)
make check      # everything CI runs: format check, vet, tests with the race detector, coverage gate
make check-linux  # the same on Linux with real FUSE mounts, in a container (needs Docker)
make lint         # golangci-lint
```

## Repository layout

```
server/     the server program (splicefsd) and its packages
client/     the client program (splicefs), the importable client library (client/core)
            and the mount layers (client/mount)
internal/   code shared by both programs
test/       end-to-end tests that run real servers against the real client
docs/       design and configuration reference
packaging/  systemd units
scripts/    development helpers used by make and CI
```

## Versioning

SpliceFS follows [Semantic Versioning](https://semver.org). Releases are git tags of the form
`vMAJOR.MINOR.PATCH`. The server and the client are released together under one version, and
both print it with `--version`. Before 1.0.0 the API and the configuration format may change
in any minor release; every change is listed in the changelog.

## License

MIT. See [LICENSE](LICENSE).
