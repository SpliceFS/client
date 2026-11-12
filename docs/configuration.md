# Client configuration

Nothing has to be configured. A fresh install runs with no mounts and waits. You add clusters
and mounts only when you need them.

The client uses two folders:

| Folder        | Linux packages          | macOS build                 | Holds                         |
|---------------|-------------------------|-----------------------------|-------------------------------|
| Configuration | `/etc/splicefs/client`  | `~/.splicefs/client`        | what you write: settings, clusters, mounts |
| Cache         | `/var/cache/splicefs`   | `~/Library/Caches/splicefs` | cached file contents          |

`--config DIR` overrides the configuration folder. The cache folder is set by `cache.dir`,
see "Settings".

```
splicefs --config /srv/splicefs/etc mount
```

Upgrades never touch the files you put in the configuration folder. The package creates three
empty folders in it (`conf.d`, `clusters.d`, `mounts.d`) and no files. A missing folder is the
same as an empty one.

The format may change before the first release.

## Settings

Settings are files in `conf.d/` inside the configuration folder. Today the only settings are
the `cache` keys that every mount inherits.

- Only files whose name ends in `.yaml` are read. Names that start with a dot, folders and all
  other files are ignored, so editor backups and temporary files of sync tools do no harm.
- Files are read in name order (byte order, so `10-a.yaml` comes before `20-b.yaml`). A later
  file wins over an earlier one.
- Maps merge key by key. A single value in a later file replaces the earlier one.
- Each file is checked on its own. An unknown key, or more than one YAML document in a file,
  is an error that names the file.
- After merging, the result is validated and every problem is reported together. A problem
  names the file its value came from.
- Relative paths in a file (`cache.dir`, and `password_file` in a cluster) are relative to the
  configuration folder.
- Sizes take the suffixes `KiB`, `MiB`, `GiB` and `TiB`, or a whole number of bytes.
  Durations take the suffixes `s`, `m`, `h` and `d` (one day is 24 hours). Where a table below
  says so, `unlimited` is also accepted.

This file sets every key to its default. You do not need to write any of it.

```yaml
# /etc/splicefs/client/conf.d/00-example.yaml

# Where and how file contents are cached. The folder must not be a mount target, lie inside
# one, or contain one.
cache:
  dir: /var/cache/splicefs  # on macOS: ~/Library/Caches/splicefs
  max_size: 10GiB           # evict least recently read files beyond this; "unlimited" never evicts for size
  max_age: unlimited        # evict files not read for this long; "unlimited" never evicts for age
  min_free: 1GiB            # always leave this much free on the cache disk; blocks are not stored below it
  min_file_size: 0          # files smaller than this are never cached
  revalidate: 1h            # how long a cached file is trusted before it is checked against the nodes again
  listing_ttl: 30s          # how long a directory listing is reused
```

| Key                | Meaning                                              | Default            | Limits |
|--------------------|------------------------------------------------------|--------------------|--------|
| `cache.dir`        | Folder for cached file contents                      | `/var/cache/splicefs`, on macOS `~/Library/Caches/splicefs` | absolute path (a relative one is read relative to the configuration folder); must not be, lie inside or contain a mount target |
| `cache.max_size`   | Total size beyond which the least recently read files are evicted | `10GiB` | at least one block of the largest cluster, or `unlimited` |
| `cache.max_age`    | Files not read for this long are evicted             | `unlimited`        | above `0`, or `unlimited` |
| `cache.min_free`   | Free space always left on the cache disk             | `1GiB`             | a finite size |
| `cache.min_file_size` | Files smaller than this are never cached          | `0`                | a finite size |
| `cache.revalidate` | How long a cached file is trusted before it is checked against the nodes again | `1h` | a finite duration above `0` |
| `cache.listing_ttl`| How long a directory listing is reused               | `30s`              | a finite duration of at least `0`; `0` turns reuse off |

Notes on the table:

- For a permanent local copy of everything that is read, set `max_size: unlimited` and
  `max_age: unlimited`.
- A mount may override any of these keys, see "Mounts". Mounts that share a cache folder share
  cached blocks.
- The cache folder of a mount is created when the mount is made.

## Clusters

A cluster is a group of servers that the client logs in to. Each cluster is one file in
`clusters.d/` inside the configuration folder. The file name without `.yaml` is the cluster
name, and mounts refer to the cluster by that name. The same rules as for `conf.d/` apply to
which files are read. No clusters is valid.

A cluster name has lower-case letters, digits, `.`, `_` and `-`, starts with a letter or digit,
and has at most 64 characters. A `name` key inside the file is not allowed, because the file
name is the name.

| Key                | Meaning                                                | Default   | Limits |
|--------------------|--------------------------------------------------------|-----------|--------|
| `url`              | Address of any node, or of a name that resolves to several | required | absolute `http` or `https` URL with a host, no `?` or `#`; a trailing `/` is dropped |
| `user`             | Login name                                             | required  | not empty |
| `password_file`    | File that holds the password                           | required  | not empty; a relative path is relative to the configuration folder |
| `block_size`       | Size of the byte ranges that are requested and cached  | `5MiB`    | `64KiB` to `1GiB` |
| `streams_per_node` | Requests in flight to one node at the same time        | `4`       | 1 to 64 |
| `read_ahead`       | How far ahead of the reader blocks are requested       | `64MiB`   | a finite size, at least one block |

```yaml
# /etc/splicefs/client/clusters.d/east.yaml
url: https://east.example.com:8480/api   # any node, or a name that resolves to several
user: alice
password_file: secrets/east              # relative to the configuration folder
block_size: 5MiB
streams_per_node: 4
read_ahead: 64MiB
```

The password file holds the password and nothing else. A newline at the end is ignored. Keep
the file readable only by the user the client runs as:

```
chmod 600 /etc/splicefs/client/secrets/east
```

The client reads the password files when it starts and when it reloads. `splicefs config check`
does not read them.

## Mounts

A mount makes a folder of a cluster appear at a local folder. Each mount is one file in
`mounts.d/` inside the configuration folder. The file name without `.yaml` is the mount name.
The same rules as for `conf.d/` apply to which files are read, and the same rules as for
cluster names apply to mount names. Mounts are handled in name order. No mounts is valid.

| Key           | Meaning                                                   | Default  | Limits |
|---------------|-----------------------------------------------------------|----------|--------|
| `cluster`     | Cluster to mount from                                     | required | the name of a file in `clusters.d/` |
| `path`        | Folder of the cluster to mount                            | `/`      | starts with `/`, no `..` |
| `target`      | Local folder to mount at                                  | required | absolute path |
| `allow_other` | Let users other than the one running the client read the mount | `false` | `true` or `false`; on Linux needs `user_allow_other` in `/etc/fuse.conf` |
| `offline`     | When no node can be reached, keep serving files that are completely cached | `true` | `true` or `false`; `false` makes those reads fail |
| `cache`       | Cache keys for this mount only                            | the settings | the same keys and limits as `cache` in "Settings" |

With `path: /` every share of the cluster appears as a folder. A longer path mounts only that
folder of the cluster.

```yaml
# /etc/splicefs/client/mounts.d/files.yaml
cluster: east
path: /                  # the cluster root: every share appears as a folder
target: /mnt/files
```

```yaml
# /etc/splicefs/client/mounts.d/movies.yaml
cluster: east
path: /media/movies      # or any subfolder
target: /srv/movies
allow_other: true
offline: true
cache:                   # only the keys you write differ from the settings
  dir: /data/cache/movies
  max_size: 1TiB
```

The `cache` section of a mount names only the keys that differ. Every key you leave out is
taken from the settings in `conf.d/`, or from the default.

Validation enforces these rules about targets and cache folders. It reports every violation
together:

- Two mounts must not have the same target.
- A target must not lie inside the target of another mount, and must not contain one. A mount
  over a folder that holds another mount would hide it.
- A cache folder, whether from the settings or from a mount, must not be a mount target, lie
  inside one, or contain one. Otherwise the client would cache the files it serves into the
  mount, or hide the cache under it.
- Mounts that use the same cache folder must set the same `max_size`, `max_age` and
  `min_free`, because a folder has one eviction policy. The client checks this when it starts
  and when it reloads, and `splicefs config check` does not.

## Mounting with mount and fstab

On Linux, `mount -t splicefs` and `/etc/fstab` lines of type `splicefs` work too. A mount file
in `mounts.d/` is the normal way to define a mount and is what to use first. Use `mount` and
fstab if you keep all your mounts in fstab, or for a one-off mount.

The helper is `/usr/sbin/mount.splicefs`, a link to `/usr/bin/splicefs` that the packages
install. `mount` runs it, and it only asks the running client for the mount. The client makes
the mount, as it does for a mount file. So the client must be running, and the target folder
must exist.

The source is `<cluster>:<path>`. The cluster is the name of a file in `clusters.d/`, and the
path is a folder of that cluster. `east:/` is the whole cluster.

```
mount -t splicefs east:/media /mnt/media
mount -t splicefs east:/media /mnt/media -o allow_other
umount /mnt/media
```

A cluster that is not defined is refused. `mount` exits with status 32 and the helper prints
the reason, for example that the cluster is not defined. If the client is not running, the
message says to start the service. Bad arguments exit with status 1.

An fstab line:

```
east:/media  /mnt/media  splicefs  _netdev,nofail,x-systemd.requires=splicefs.service  0 0
```

`x-systemd.requires=splicefs.service` makes systemd start the service before it runs the
helper for this line. The helper does not start the client, it only asks it. Without this
option, at boot the helper can run before the client is up, and the mount fails. systemd reads
the `x-` option and the helper ignores it.

Options:

| Option                 | Effect |
|------------------------|--------|
| `allow_other`          | Same as the mount file key, see "Mounts" |
| `offline`, `nooffline` | Same as the mount file key `offline`. The last one given wins |
| `rw`, `async`, `ro`, `defaults`, `auto`, `noauto`, `nofail`, `_netdev`, `user`, `nouser`, `users`, `owner`, `exec`, `noexec`, `suid`, `nosuid`, `dev`, `nodev`, `atime`, `noatime`, `relatime` | Accepted and ignored, so that a usual fstab line works as written |
| `x-...`, `comment=...` | Accepted and ignored. Other programs read them |

Any other option is an error that names it. `rw` is accepted and ignored because `mount`
passes it to the helper by default. The mount is always read-only, whatever is asked. The
flags `-s`, `-f`, `-n` and `-v` are accepted and ignored. A mount made this way uses the cache
settings from `conf.d/` and has no `cache` section of its own.

A mount made this way is a requested mount. The client keeps a list of them:

- On a restart of the service, the requested mounts come back. A mount that cannot be made
  again is logged and dropped.
- On a reboot, the list is cleared, because it is kept in the runtime directory next to the
  control socket. fstab asks again.
- On `umount`, the mount is taken off the list.
- On a reload, a requested mount stays. It is dropped, with a warning in the log, if its
  cluster is gone or if its target now overlaps a mount file. The rules in "Mounts" apply to
  requested mounts too.

The control socket is readable only by the user the client runs as. So the service must run
as root, which the packages do, and `mount` must be run by root, or by that user. By default
the helper finds the client of `/etc/splicefs/client`. To use another configuration folder,
set `SPLICEFS_CONFIG` to it in the environment of `mount`.

## Applying changes

The client applies changes to `conf.d/`, `clusters.d/` and `mounts.d/` without a restart. Send
it `SIGHUP`:

```
systemctl reload splicefs         # Linux packages with systemd
rc-service splicefs reload        # Alpine
kill -HUP <pid>                   # by hand, and on macOS
```

Run `splicefs config check` first. It finds mistakes in the files before the client reads them.

- The client reads and checks the whole configuration, reads the password files and opens the
  cache folders before it changes anything. If any of that fails, nothing changes. The client
  keeps running on the configuration it had, and the log has a line `configuration not applied`
  with the reason. On success the log has `configuration applied` and the number of mounts.
- A mount file that was added is mounted.
- A mount file that was removed is detached at once: the mount point disappears. Files that
  are open keep working until they are closed.
- A mount whose file changed is mounted again. So is a mount whose cluster file changed, or
  whose cache keys changed (from its own `cache` section or from the settings). Renaming a
  mount file also counts as a change.
- A mount with nothing changed is not touched.
- A cluster whose file did not change keeps its login and its measured node speeds. A changed
  cluster is built new. The old one stays in use for as long as a detached mount still has
  files open on it.
- A cache folder that is already open stays open and keeps its files. New limits apply at the
  next eviction pass.
- A mount that cannot be made, for example because its target is missing or its cluster
  rejects the user or password, is logged and skipped. The other mounts are still applied.
  A cluster that cannot be reached is only a warning, and its mounts are made anyway, as at
  start.
- A mount that cannot be detached is logged (`mount not detached`) and stays as it is.
- A mount that you unmounted by hand, with `splicefs umount` or with `umount`, stays unmounted
  until the next reload or restart. A reload mounts it again if its file is still there.
- The client keeps running when the last mount is gone, and when there never was one.
- If you started the client with targets (`splicefs mount /mnt/files`), a reload looks only at
  mount files with those targets.

A password file that cannot be read, two mounts that share a cache folder but set different
limits, and a cache folder that cannot be opened all make the whole reload fail, because they
are found before anything changes.

## Checking

Check the configuration before you start the client or reload it:

```
splicefs config check
```

This prints `ok` with the number of clusters and mounts and exits with status 0. If something
is wrong, it prints every problem, names the file each came from, and exits with status 1.

```
ok: 1 clusters, 1 mounts
```

```
splicefs: invalid client config /etc/splicefs/client: mounts.bad.cluster: "west" is not a cluster defined in clusters (file mounts.d/bad.yaml)
mounts.bad.target: /mnt/files/sub is inside the target /mnt/files of another mount (mounts.all) (file mounts.d/bad.yaml)
```

It does not read the password files, and it does not check that targets and cache folders
exist or that a cluster can be reached. The client reports those problems when it starts or
reloads.

```
splicefs config show
```

This prints the effective configuration as YAML: the cache settings, the clusters, then the
mounts. A value that a file set ends with a comment naming that file. A value without a
comment is a default. Relative paths are shown in full, and each mount shows its whole `cache`
section, with the settings it inherited.

```
cache:
  dir: /srv/splicefs/cache # conf.d/10-cache.yaml
  max_size: 200GiB # conf.d/10-cache.yaml
  max_age: 30d # conf.d/10-cache.yaml
  min_free: 1GiB
  min_file_size: "0"
  revalidate: 1h
  listing_ttl: 30s
clusters:
  east:
    url: https://east.example.com:8480/api # clusters.d/east.yaml
    user: alice # clusters.d/east.yaml
    password_file: /etc/splicefs/client/secrets/east # clusters.d/east.yaml
    block_size: 5MiB
    streams_per_node: 4
    read_ahead: 64MiB
mounts:
  - name: media
    cluster: east # mounts.d/media.yaml
    path: /media # mounts.d/media.yaml
    target: /mnt/media # mounts.d/media.yaml
    allow_other: false
    offline: true
    cache:
      dir: /srv/splicefs/cache
      max_size: 1TiB # mounts.d/media.yaml
      max_age: 30d
      min_free: 1GiB
      min_file_size: "0"
      revalidate: 1h
      listing_ttl: 30s
```

Both commands take `--config DIR`.

## Commands

```
splicefs [--config DIR] [--log-level LEVEL] mount [TARGET...]
        mount every file in mounts.d, or only the named targets; stays in the foreground, with
        or without mounts; SIGHUP applies the configuration; SIGINT or SIGTERM unmounts
        everything and exits
splicefs [--config DIR] umount [--force] [TARGET...]
        ask the running client to unmount; a mount in use is refused unless --force is given;
        the client keeps running
splicefs [--config DIR] status [--json]
        mounts, nodes with their measured speed, cache use
splicefs [--config DIR] config check
splicefs [--config DIR] config show
splicefs --version
```

`--log-level` takes `debug`, `info`, `warn` or `error`. The client logs to its standard error
output, which is the journal under systemd and `/var/log/splicefs.log` under OpenRC.

`mount` fails at once if a cluster rejects the user or password, or if a mount cannot be made
(a reload only logs these and skips the mount). If a cluster cannot be reached, it mounts anyway
and the mount starts working when the cluster returns, so a machine can boot before its network
is up.

## Starting at boot

The ready-made unit is `splicefs.service` in [`packaging/systemd`](../packaging/systemd). The unit:

```ini
# /etc/systemd/system/splicefs.service
[Unit]
Description=SpliceFS mounts
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/splicefs mount
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
CacheDirectory=splicefs

[Install]
WantedBy=multi-user.target
```

The client tells systemd when every mount is up, so units ordered after it can rely on the
mounts. With nothing configured it reports ready at once and runs without mounts. Stopping the
unit unmounts everything. Running `splicefs mount` by hand does the same job without the unit.

The configuration of the server is described in the
[server configuration reference](https://github.com/SpliceFS/server/blob/main/docs/configuration.md).
