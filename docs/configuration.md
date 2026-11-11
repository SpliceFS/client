# Client configuration

The client reads a configuration directory. The default is `/etc/splicefs/client`, and
`--config DIR` overrides it.

The main file in the directory is `config.yaml`. Relative paths inside a configuration
file are relative to the configuration directory. Sizes take the suffixes `KiB`, `MiB`,
`GiB`, `TiB`; durations take `s`, `m`, `h`, `d`.

The format may change before the first release.

```yaml
# /etc/splicefs/client/config.yaml

# Where and how file contents are cached. The directory must not be a mount target,
# lie inside one, or contain one.
cache:
  dir: /var/cache/splicefs
  max_size: 200GiB      # evict least recently read files beyond this; "unlimited" never evicts for size
  max_age: 30d          # evict files not read for this long; "unlimited" never evicts for age
  min_free: 10GiB       # always leave this much free on the cache disk; blocks are not stored below it
  min_file_size: 0      # files smaller than this are never cached
  revalidate: 1h        # how long a cached file is trusted before it is checked against the nodes again
  listing_ttl: 30s      # how long a directory listing is reused

# Permanent local copies of everything that is read:
#   max_size: unlimited
#   max_age: unlimited

clusters:
  example:
    url: https://files.example.com/api   # any node, or a name that resolves to several
    user: alice
    password_file: secrets/example       # mode 600
    block_size: 5MiB
    streams_per_node: 4
    read_ahead: 64MiB

mounts:
  - cluster: example
    path: /                 # the cluster root: every share appears as a folder
    target: /mnt/files
  - cluster: example
    path: /media/movies     # or any subdirectory
    target: /srv/movies
    allow_other: true       # let other users read the mount; needs user_allow_other in /etc/fuse.conf
    offline: true           # when no node can be reached, keep serving files that are completely
                            # cached (default true); false makes those reads fail instead
    # cache:                # a mount may override any cache setting, for example to use another disk
    #   dir: /data/cache/movies
    #   max_size: 1TiB
```

Mounts that share a cache directory share cached blocks.

## Commands

```
splicefs [--config DIR] [--log-level LEVEL] mount [TARGET...]
        mount every entry in the configuration, or only the named targets; stays in the
        foreground; SIGINT or SIGTERM unmounts everything and exits
splicefs [--config DIR] umount [--force] [TARGET...]
        ask the running client to unmount; a mount in use is refused unless --force is given;
        the client exits when its last mount is gone
splicefs [--config DIR] status [--json]
        mounts, nodes with their measured speed, cache use
splicefs --version
```

`mount` fails at once if a cluster rejects the user or password. If a cluster cannot be
reached, it mounts anyway and the mount starts working when the cluster returns, so a
machine can boot before its network is up.

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
Restart=on-failure
CacheDirectory=splicefs

[Install]
WantedBy=multi-user.target
```

The client tells systemd when every mount is up, so units ordered after it can rely on the
mounts. Stopping the unit unmounts everything. Running `splicefs mount` by hand does the
same job without the unit.

The configuration of the server is described in the
[server configuration reference](https://github.com/SpliceFS/server/blob/main/docs/configuration.md).
