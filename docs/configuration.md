# Configuration

Both programs read a configuration directory:

| Program | Default directory | Override |
|---|---|---|
| `splicefsd` (server) | `/etc/splicefs/server` | `--config DIR` |
| `splicefs` (client) | `/etc/splicefs/client` | `--config DIR` |

The main file in either directory is `config.yaml`. Relative paths inside a configuration
file are relative to the configuration directory. Sizes take the suffixes `KiB`, `MiB`,
`GiB`, `TiB`; durations take `s`, `m`, `h`.

This is the planned format. It is not implemented yet and may change before the first release.

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
  token_ttl: 30m
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

cache:
  dir: /var/cache/splicefs
  max_size: 200GiB

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
```

### Commands

```
splicefs mount [TARGET...]    mount every entry in the configuration, or only the named targets;
                              stays in the foreground and unmounts on exit
splicefs umount [TARGET...]   ask the running client to unmount
splicefs status               mounts, nodes and their measured speed, cache use
```

### Starting at boot

```ini
# /etc/systemd/system/splicefs.service
[Unit]
Description=SpliceFS mounts
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/splicefs mount
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Stopping the unit unmounts everything. Running `splicefs mount` by hand does the same job
without the unit.
