# Configuration

Both programs read a configuration directory:

| Program | Default directory | Override |
|---|---|---|
| `splicefsd` (server) | `/etc/splicefs/server` | `--config DIR` |
| `splicefs` (client) | `/etc/splicefs/client` | `--config DIR` |

The main file in either directory is `config.yaml`. Relative paths inside a configuration
file are relative to the configuration directory. Sizes take the suffixes `KiB`, `MiB`,
`GiB`, `TiB`; durations take `s`, `m`, `h`, `d`.

The format may change before the first release.

## Server

```yaml
# /etc/splicefs/server/config.yaml

# Where to listen. One entry per address and port.
listen:
  - address: 192.0.2.10     # the address of one interface; 0.0.0.0 or :: for all
    port: 8480
    # tls:                  # only when no reverse proxy terminates TLS
    #   cert_file: tls/fullchain.pem
    #   key_file: tls/privkey.pem

# URL prefix the API is served under.
base_path: /api

# Reverse proxies whose X-Forwarded-For header is believed.
trusted_proxies: [192.0.2.1]

# Directories to share. The key is the share's name and its top-level folder for readers.
shares:
  media:
    path: /srv/media
    exclude: [".*"]         # glob patterns for names never listed or served
    min_age: 0s             # ignore files modified more recently than this
    follow_symlinks: false  # links that stay inside the share may be followed when true

cluster:
  name: example
  node: east                          # this node's name in the node list
  nodes_file: /srv/shared/nodes.yaml  # the same file on every node

auth:
  users_dir: /srv/shared/users        # one <user>.yaml per user
  keys_file: /srv/shared/keys.yaml    # token signing keys, mode 600
  token_ttl: 30m                      # checked with 30s of leeway; keep node clocks in step with NTP
  login_rate: 10/m                    # attempts per client address

mtime_precision: 1s   # modification times are compared at this precision

log:
  level: info         # debug, info, warn, error
  format: text        # text or json
```

### Node list

```yaml
# nodes.yaml
cluster: example
nodes:
  - name: east
    url: https://east.example.com/api
  - name: west
    url: https://west.example.com/api
```

### User file

```yaml
# users/alice.yaml
password_hash: "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>"
shares: [media]     # ["*"] allows every share
disabled: false
```

`splicefsd hash-password` reads a password from standard input and prints the hash.

### Keys file

```yaml
# keys.yaml: the first key signs, every key verifies
keys:
  - id: 2026-11
    secret: "<base64, at least 32 random bytes>"
```

## Client

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

### Commands

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

### Starting at boot

Ready-made units are in [`packaging/systemd`](../packaging/systemd). The client unit:

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
