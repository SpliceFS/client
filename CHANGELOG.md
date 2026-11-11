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
