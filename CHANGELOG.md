# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Repository setup: design and configuration documents, contribution process, CI.
- `splicefs --version`.
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
