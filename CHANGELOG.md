# Changelog

All notable changes to this project are recorded here. Every merge into `main` is a
release with its own version, so there are no version headings: the
[release page](../../releases) of a version names its change, and this file collects the
notable ones.

## Changes

### Added

- Repository setup: design and configuration documents, contribution process, CI.
- `splicefs --version`.
- Fuzz tests for the configuration, server responses, the merged directory tree, the cache
  metadata and the block arithmetic, and `make fuzz` to run them.
- Client (`splicefs`): `mount`, `umount` and `status`; several clusters and mounts in one
  configuration; merged directory tree; block reads from every node holding an identical
  copy, with per-node speed tracking and re-request of late blocks; on-disk block cache
  with size, age and free-space limits and revalidation; reading of completely cached
  files while no node can be reached; systemd readiness notification.
- Mount layers: FUSE on Linux, a local NFS server with the built-in NFS client on macOS.
- `core`, the client as an importable library (`github.com/SpliceFS/client/core`).
- A systemd unit for the program.
- Packages named `splicefs-client` for amd64 and arm64: deb (Debian, Ubuntu), rpm (Red Hat
  family) and apk (Alpine). Each installs the program and the service for its system
  (systemd, or OpenRC on Alpine), depends on `fuse3` and creates the empty folders `conf.d`,
  `clusters.d` and `mounts.d` in `/etc/splicefs/client`. The service is enabled and started on
  install, and an upgrade never touches the files an administrator put in those folders.
- `make package-test` install-tests the packages on every supported Debian, Ubuntu, Rocky
  Linux, Amazon Linux, openSUSE and Alpine version, listed in `packaging/test-systems`.
- Signed apt, rpm and Alpine repositories built from the packages (`make repo`), with a test
  that installs from each on every supported system with signature checks on
  (`make repo-test`).
- Release and publish: a `vX.Y.Z` tag builds, signs, tests and publishes the package
  repositories to `packages.splicefs.org/client` (`make publish-fetch`, `make publish`,
  `make publish-check`) and creates a GitHub release with the packages, `SHA256SUMS` and its
  signature (`make release-assets`). Pre-release tags such as `v1.2.0-rc.1` give versions that
  sort before the release in deb, rpm and apk; `make publish-test` checks the publishing
  without a server.
- The client runs with nothing configured: no clusters and no mounts. It keeps running, with
  or without mounts, until it is stopped. A mount unmounted by hand stays unmounted until the
  next reload or restart.
- `splicefs config check` validates the configuration and prints every problem with the file
  it came from. `splicefs config show` prints the effective configuration with the file each
  value came from. Both take `--config DIR`.
- `SIGHUP` applies the configuration without a restart (`systemctl reload splicefs`,
  `rc-service splicefs reload`). Added mounts are mounted, removed ones are detached at once
  while open files keep working until they are closed, changed ones are mounted again, and
  unchanged ones are not touched. A mount that cannot be made is logged and the others are
  still applied. A configuration with an error is refused and only logged.
- Default folders for a macOS build: `~/.splicefs/client` for the configuration and
  `~/Library/Caches/splicefs` for the cache.
- Mount helper for Linux: `mount -t splicefs CLUSTER:PATH TARGET` and `/etc/fstab` lines of
  type `splicefs` ask the running client to make the mount. The packages install it as
  `/usr/sbin/mount.splicefs`, a link to the program. Mounts made this way come back when the
  service restarts, and are cleared on a reboot. See "Mounting with mount and fstab" in the
  configuration reference.
- Health check: every 10 seconds the client looks up the reserved name `.splicefs-probe` in
  the root of every mount. The client answers it itself and asks no server. A mount that fails
  three checks in a row (no answer within 5 seconds, or a wrong answer) is force unmounted
  and mounted again, and the log has `mount does not answer, mounting it again`. On Linux the
  client then cuts the kernel connection of the hung mount, so programs waiting on it get an
  error and do not wait forever. A mount that a reload detaches is not cut: its open files
  keep working.
- Heartbeat: after every round of checks the client sends `WATCHDOG=1` to systemd, and the
  packaged unit has `WatchdogSec=300`. A client whose run loop is stuck for five minutes is
  killed and started again. On Alpine (OpenRC) there is no heartbeat.
- Trunk development: one branch, `main`, and one version per change.
- `make service-test`: on every supported system the package is installed under that system's
  real service manager (systemd or OpenRC as process 1 of a container) and the service must
  start by itself, answer `splicefs status`, come back after a reboot and stop when the
  package is removed.
- FreeBSD: the client runs on FreeBSD 14 and 15, with the same FUSE mounts as on Linux.
  `make check-freebsd` runs the tests in a FreeBSD virtual machine, and `make package-freebsd`
  builds, installs and tests the package there.
- `make lint` also runs ShellCheck, with every optional check on; the two that are off say why
  in `.shellcheckrc`. `make check` fails when a compiled program or a file over 1 MiB is
  tracked. The FreeBSD scripts are the same files as the server's again.
- Code quality checks at their strictest: every linter is on, and each one that is off says
  why in `.golangci.yml`. The code declares its errors, so callers can test for them.
  Behavior, the configuration keys and the messages are unchanged, with one exception: when
  the answer to `--help` or `--version` cannot be written, the program now says so and exits
  with status 1.
- `make check` fails when a tracked file names a private machine, account or place, and the
  ignore rules cover programs built in place, editor leftovers and disk images.
- FreeBSD packages are published as a signed package repository
  (`https://packages.splicefs.org/client/freebsd/${ABI}`).

### Changed

- **Breaking:** the configuration is a set of files instead of one `config.yaml`. Settings go
  in `conf.d/*.yaml`, read in name order and merged (today these are the `cache` defaults),
  each cluster is one file in `clusters.d/` whose file name is the cluster name, and each mount
  is one file in `mounts.d/` whose file name is the mount name. The top-level keys `clusters`
  and `mounts` are gone. A configuration written for an earlier build must be rewritten.
- **Breaking:** the default cache folder is `/var/cache/splicefs` (`~/Library/Caches/splicefs`
  on macOS) instead of a required setting.
- **Breaking:** `splicefs mount` no longer exits when its last mount ends, and it starts
  without a configuration.
- Packages start the service on install even when nothing is configured.
- The client logs in, renews its token and asks for the node list at the cluster host, every
  minute instead of every 5 minutes. If the cluster host cannot be reached, the client treats
  the cluster as unreachable, keeps the node hosts it knows until its login runs out, and
  recovers by itself when the cluster host answers again.
- A cluster that lists no nodes is used through its own address, so a single server works
  without setting its public address.

### Fixed

- macOS: the client now starts and mounts while no server can be reached, as it already did
  on Linux. The mount appears, listings fail with an input/output error until a server
  answers, and then it recovers by itself.
- Tests: the macOS mount test client in `mount/nfs` picked a random local port itself and
  failed with "address already in use" when runs overlapped; it now asks again like the
  other test client, so concurrent test runs no longer collide.
- The package and service tests download each test image with up to 3 tries and name a failed
  download as one; a service manager that did not start shows the container's own output.

### Security

- Entry names that a node lists are checked. A name that is empty, a dot segment, or holds a
  slash or a NUL byte is dropped, so a hostile node cannot make an entry point outside the
  directory it was listed in. Found by the new fuzz tests.

### Removed

- openSUSE Leap 15.6 is no longer supported: its vendor ended its support on 2026-04-30. A
  system is supported only while its own vendor supports it.
