# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org).

## [Unreleased]

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
  (systemd, or OpenRC on Alpine), depends on `fuse3` and creates an empty
  `/etc/splicefs/client`. The service is enabled and starts once a configuration exists.
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

### Security

- Entry names that a node lists are checked. A name that is empty, a dot segment, or holds a
  slash or a NUL byte is dropped, so a hostile node cannot make an entry point outside the
  directory it was listed in. Found by the new fuzz tests.
