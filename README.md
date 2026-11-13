# SpliceFS client

[![CI](https://github.com/SpliceFS/client/actions/workflows/ci.yml/badge.svg)](https://github.com/SpliceFS/client/actions/workflows/ci.yml)
[![Security](https://github.com/SpliceFS/client/actions/workflows/security.yml/badge.svg)](https://github.com/SpliceFS/client/actions/workflows/security.yml)
[![Coverage](https://codecov.io/gh/SpliceFS/client/graph/badge.svg)](https://codecov.io/gh/SpliceFS/client)
[![Quality gate](https://sonarcloud.io/api/project_badges/measure?project=SpliceFS_client&metric=alert_status)](https://sonarcloud.io/summary/overall?id=SpliceFS_client)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/SpliceFS/client/badge)](https://scorecard.dev/viewer/?uri=github.com/SpliceFS/client)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/16606/badge)](https://www.bestpractices.dev/projects/16606)
[![Go Reference](https://pkg.go.dev/badge/github.com/SpliceFS/client.svg)](https://pkg.go.dev/github.com/SpliceFS/client)
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
- **Replication is not part of SpliceFS yet.** Today, use Syncthing, rsync, a clustered
  filesystem, or nothing at all: a cluster of one node works. SpliceFS only requires that a
  file with the same path, size and modification time has the same content on every node.
  Reading is the first step; writing and built-in redundancy are planned for later versions.

## Features

- Read-only mount on Linux (FUSE) and macOS (through the built-in NFS client)
- Nothing has to be configured: a fresh install runs with no mounts, and packages start the
  service on install
- Settings, clusters and mounts are small files in folders that upgrades never touch; one
  mount per file, and a cluster root or any subdirectory can be mounted anywhere
- `splicefs config check` validates the files; `splicefs config show` prints the result and
  the file each value came from
- Applies a changed configuration on `SIGHUP` without a restart: new mounts appear, removed
  ones are detached at once, files that are open keep working until they are closed
- On Linux, `mount -t splicefs` and `/etc/fstab` lines of type `splicefs` work too: the helper
  asks the running client to make the mount
- Repairs itself: every mount is checked every 10 seconds and a mount that stops answering is
  mounted again; on systemd the client sends a heartbeat and is restarted if its loop gets stuck
- Several clusters and several mounts in one client
- Multi-source block reads with per-node speed tracking and re-request of late blocks
- Local block cache with size, age and free-space limits, kept across restarts; files that
  were read completely stay readable when every node is unreachable

## Quick start

The nodes must be running first. See the server's
[quick start](https://github.com/SpliceFS/server#quick-start).

On the reader, install the package for your system. The service starts at once and keeps
running. No cluster and no mount is configured yet, so nothing is mounted.

Add a cluster. The file name is the cluster name. `password_file` is relative to the
configuration folder:

```
# /etc/splicefs/client/clusters.d/example.yaml
url: "https://east.example.com/api"
user: alice
password_file: secrets/example
```

Put the password in that file and make the target folder:

```
install -d -m 700 /etc/splicefs/client/secrets
(umask 077; cat > /etc/splicefs/client/secrets/example)   # type the password, then Ctrl-D
mkdir -p /mnt/files
```

Add a mount. The file name is the mount name:

```
# /etc/splicefs/client/mounts.d/files.yaml
cluster: example
path: /
target: /mnt/files
```

Check the files and apply them without a restart:

```
splicefs config check
systemctl reload splicefs        # on Alpine: rc-service splicefs reload
splicefs status                  # mounts, node speeds, cache use
```

The files of the cluster appear in `/mnt/files`. To stop a mount, delete its file and reload,
or run `splicefs umount /mnt/files`; a mount unmounted by hand stays unmounted until the next
reload or restart.

On Linux you can also mount with `mount` or fstab instead of a mount file. The client must be
running, and the cluster must be defined in `clusters.d`:

```
mount -t splicefs example:/ /mnt/files
echo 'example:/ /mnt/files splicefs _netdev,nofail,x-systemd.requires=splicefs.service 0 0' >> /etc/fstab
```

The cache is in `/var/cache/splicefs` unless a file in `conf.d/` says otherwise. On a macOS
build the folders are `~/.splicefs/client` and `~/Library/Caches/splicefs`, and there is no
service: run `splicefs mount`, which stays in the foreground, and send it `SIGHUP` to apply a
changed configuration. The [configuration reference](docs/configuration.md) lists every
setting.

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
make package      # deb, rpm and apk packages for amd64 and arm64 in ./dist (needs Docker)
make package-test # installs and removes each package in a clean container of every system
make repo         # signed apt, rpm and Alpine repositories in ./dist/repo from the packages in ./dist
make repo-test    # installs from the repositories with signature checks on, in every system
make publish-fetch  # downloads the published repositories into ./dist/repo
make publish        # uploads ./dist/repo to packages.splicefs.org/client, never deleting a package
make publish-check  # lists the remote root, changes nothing
make publish-test   # checks publishing and release-assets against local folders, no server
make release-assets # package files, SHA256SUMS and its signature for the GitHub release in ./dist/release
```

`make package-test` covers every system listed in `packaging/test-systems`. From an arm64
machine, `make package-test PLATFORM=linux/amd64` tests the other CPU type by emulation.

## Repository layout

```
cmd/splicefs/  the splicefs program: the mount, umount, status and config commands
app/           builds the client from the configuration, mounts it, serves the control socket
               and applies a changed configuration on SIGHUP
config/        loads and validates the files in conf.d, clusters.d and mounts.d
core/          the client as an importable library (github.com/SpliceFS/client/core)
mount/         the contract every mount layer offers; mount/fuse is Linux and FreeBSD, mount/nfs is macOS
internal/      node HTTP client, cluster session, merged tree, block cache, block reader, and
               the version number set at build time
docs/          design and configuration reference
packaging/     systemd unit, OpenRC script, package files
scripts/       development helpers used by make and CI
```

The JSON types of the HTTP API (`wire`) and the size and duration types of the configuration
(`units`) come from the [common](https://github.com/SpliceFS/common) module, which the server
uses too.

## Versioning

SpliceFS follows [Semantic Versioning](https://semver.org). Releases are git tags of the form
`vMAJOR.MINOR.PATCH` in this repository, and `splicefs --version` prints the version. The
server and the client are versioned separately: each repository has its own version tags. A
client works with a server that serves the API version the client speaks, and that version is
part of the URL (`/v1/`). Before 1.0.0 the API and the configuration format may change in any
minor release; every change is listed in the changelog.

## License

MIT. See [LICENSE](LICENSE).
