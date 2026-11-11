# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Repository setup: design and configuration documents, contribution process, CI.
- `splicefsd --version` and `splicefs --version`.
- Server (`splicefsd`): HTTP API version 1 with health, login, node list, directory
  listings, file reads by byte range, head digests and free space; user files with
  argon2id password hashes; signed tokens with key rotation; login rate limiting; optional
  TLS; `splicefsd hash-password`.
- Client (`splicefs`): `mount`, `umount` and `status`; several clusters and mounts in one
  configuration; merged directory tree; block reads from every node holding an identical
  copy, with per-node speed tracking and re-request of late blocks; on-disk block cache
  with size, age and free-space limits and revalidation; reading of completely cached
  files while no node can be reached; systemd readiness notification.
- Mount layers: FUSE on Linux, a local NFS server with the built-in NFS client on macOS.
- `client/core`, the client as an importable library.
- systemd units for both programs.
