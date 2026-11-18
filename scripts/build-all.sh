#!/bin/sh
# Compiles every program in cmd/ for every system and CPU type the project ships, into
# OUTPUT_DIR/<os>-<arch>/<program>. It is the only place that compiles for the packages and
# for the release: every package format wraps these files and compiles nothing itself, so
# every format ships the same binary. LDFLAGS comes from the Makefile, which is the one place
# that works out the version. Run it from the repository top (make build-all).
set -eu

out="${1:?usage: build-all.sh OUTPUT_DIR}"
: "${LDFLAGS:?LDFLAGS is not set}"
# shellcheck source=archive-lib.sh
. scripts/archive-lib.sh
module="$(go list -m)"

# FreeBSD keeps what packages install under /usr/local and its caches under /var/cache, so
# these programs are built with those folders as their defaults (see config/paths.go).
freebsd_flags="-X $module/config.defaultConfigDir=/usr/local/etc/splicefs/${name#splicefs-} \
-X $module/config.defaultCacheDir=/var/cache/splicefs"

for os in linux freebsd darwin; do
    flags="$LDFLAGS"
    if [ "$os" = freebsd ]; then flags="$flags $freebsd_flags"; fi
    for arch in amd64 arm64; do
        mkdir -p "$out/$os-$arch"
        for command_name in $programs; do
            # CGO off makes the binary static, so one package runs on every release of every
            # distribution that takes its format, and a build for another system needs no
            # C compiler.
            CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$flags" \
                -o "$out/$os-$arch/$command_name" "./cmd/$command_name"
        done
    done
done
