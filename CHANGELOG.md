# Changelog

Every merge into `main` is a release with its own version. This file has one heading per
version, newest first, and says what that version changed for the people who install and run
the SpliceFS client, `splicefs`.

The change log of all parts of SpliceFS, side by side: https://splicefs.org/changelog/

## [0.5.11] - 2026-11-14

- The protocol names the server and client version headers and the digests a block request carries.

## [0.5.10] - 2026-11-14

- Only one splicefs process at a time may use a cache folder; a second one is refused before it
  touches the folder.

## [0.5.9] - 2026-11-14

- On macOS, a program reading a file gets one version of it from start to end, even when the file is
  replaced on the servers during the read.

## [0.5.8] - 2026-11-14

- Internal: a failed release, merge or scheduled check opens an issue that emails the owner. Nothing
  changes for users.

## [0.5.7] - 2026-11-14

- Internal: a release uploads the package files before the index files that name them, and stops at
  the first failed step. Nothing changes for users.

## [0.5.6] - 2026-11-14

- Internal: a release refuses a tag that is not on main or does not match `VERSION`. Nothing changes
  for users.

## [0.5.5] - 2026-11-14

- On macOS, `splicefs umount` removes a mount left behind by a client that crashed or was killed,
  and `splicefs mount` says to run it instead of hanging on that folder.

## [0.5.4] - 2026-11-14

- Internal: a release of the shared code now opens the pull request that moves the server to it, and
  the server's release does the same for the client.

## [0.5.3] - 2026-11-14

- Internal: each test runs once per change, on the pull request. The checks on `main` and the tests
  inside the release are gone. Nothing changes for users.

## [0.5.2] - 2026-11-14

- Internal: the long test steps report how long each of their parts took. Nothing changes for users.

## [0.5.1] - 2026-11-14

- Internal: a release now updates the Homebrew formula. Nothing changes for users.

## [0.5.0] - 2026-11-14

- The client now also compares a digest of the last 64 KiB of a file when it decides whether copies
  on different servers are the same file. Before, two copies with the same size, time and first 64
  KiB that differed further in were taken for one file, and a read could return a mix of both.
- This client needs servers of version 0.5.0 or newer, and says so when a server is older.
- After the upgrade every cached file is downloaded once more, because what identifies a copy of a
  file has changed.

## [0.4.7] - 2026-11-14

- Internal: the checks now run 35 situations with real servers, among them servers that stop during
  a read, files that change while mounted, and servers that hold different folder trees. Nothing
  changes for users.
- Internal: a release now waits for earlier releases to end, so two releases can no longer overlap.
  Nothing changes for users.

## [0.4.6] - 2026-11-14

- Internal: the checks now run the real client against three real servers, including servers that
  are down and servers that hold different files. Nothing changes for users.

## [0.4.5] - 2026-11-14

- Internal: added a check that runs the real client against released servers. Nothing changes for
  users.
- This version was not published as packages: its release started while the one before was still
  running and was stopped at the publish step. Version 0.4.6 contains it.

## [0.4.4] - 2026-11-14

- The client now refuses a server that is too old and says so, for example: "server HOST runs
  splicefsd 0.3.3, and this client (0.4.4) needs splicefsd 0.4 or newer". With the same first
  version number, a client works with every server whose second number is equal or higher.
- A server older than 0.4.0 does not report its version and is refused. Upgrade the servers to 0.4.0
  or newer before this client.

## [0.4.3] - 2026-11-13

- Documentation: the change log now has one entry per version, back to 0.1.0. All parts of SpliceFS
  are shown side by side at `splicefs.org/changelog`.

## [0.4.2] - 2026-11-13

- Internal: test downloads are retried and failures are reported more clearly. Nothing changes for
  users.

## [0.4.1] - 2026-11-13

- Documentation: now names FreeBSD where it names the other systems, and gives the FreeBSD reload
  command.

## [0.4.0] - 2026-11-13

- FreeBSD packages are published as a signed package repository, one for each FreeBSD ABI, under
  `packages.splicefs.org/client/freebsd`. It is signed with the same RSA key as the Alpine
  repository.

## [0.3.4] - 2026-11-12

- Internal: the macOS mount tests no longer fail when several test runs overlap. Nothing changes for
  users.

## [0.3.3] - 2026-11-12

- Internal: pre-release checks now reject compiled programs, large files and private names in the
  repository. Nothing changes for users.

## [0.3.2] - 2026-11-12

- Internal: the release scripts are now checked with a shell script linter. Nothing changes for
  users.

## [0.3.1] - 2026-11-12

- When the answer to `--help` or `--version` cannot be written, the program now says so and exits
  with status 1.

## [0.3.0] - 2026-11-12

- The client runs on FreeBSD 14 and 15, with the same FUSE mounts as on Linux.
- On FreeBSD the configuration is in `/usr/local/etc/splicefs/client`, the control socket is under
  `/var/run` and mounts use `noatime`. A hung mount cannot be aborted, and a forced unmount also
  ends open files.
- The FreeBSD package is not published yet.

## [0.2.4] - 2026-11-12

- openSUSE Leap 15.6 is no longer supported, because its vendor ended its support on 2026-04-30. A
  system is supported only while its own vendor supports it.

## [0.2.3] - 2026-11-12

- A cluster that lists no nodes is now used through the address in its cluster file, as its only
  node. A single server works without setting its public address, instead of failing with `0 nodes
  listed, none usable`.

## [0.2.2] - 2026-11-12

- Internal: two unreliable tests no longer depend on timing. Nothing changes for users.

## [0.2.1] - 2026-11-12

- Internal: a new test installs the package on every supported system under its real service
  manager. Nothing changes for users.

## [0.2.0] - 2026-11-12

- The client logs in, renews its token and fetches the node list at the cluster host, and it fetches
  the node list every minute instead of every 5 minutes. Only file contents, listings and statistics
  go to the node hosts.
- If the cluster host cannot be reached, the client keeps the node hosts it knows until its login
  runs out, and recovers by itself when the host answers again. Rejected credentials are final.

## [0.1.1] - 2026-11-12

- Internal: a script used to publish releases is now executable. Nothing changes for users.

## [0.1.0] - 2026-11-12

- First release of the client. `splicefs mount`, `splicefs umount` and `splicefs status` manage the
  mounts; a mount shows a merged directory tree, using FUSE on Linux and the built-in NFS client on
  macOS.
- A block cache on disk lets the client read completely cached files while no node can be reached.
  The client also starts and mounts while no server can be reached, and recovers by itself when one
  answers.
- The configuration is a set of files in `/etc/splicefs/client`: `conf.d/*.yaml`, `clusters.d/` (one
  file per cluster) and `mounts.d/` (one file per mount). `splicefs config check` and `splicefs
  config show` check and print it.
- Packages named `splicefs-client` are built for amd64 and arm64 as deb (Debian, Ubuntu), rpm (Red
  Hat family) and apk (Alpine). They install the program and its service, and signed repositories
  are at `packages.splicefs.org/client`.
