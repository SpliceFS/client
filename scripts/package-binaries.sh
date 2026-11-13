#!/bin/sh
# Compiles the program for every CPU type the packages cover, into OUTPUT_DIR/stage. The
# steps for each package format only wrap these files, so every format ships the same
# binary. It runs inside the Go container (make package).
set -eu

out="${1:?usage: package-binaries.sh OUTPUT_DIR}"
. scripts/package-lib.sh
module="$(go list -m)"

for arch in amd64 arm64; do
    mkdir -p "$out/stage/$arch"
    # CGO off makes the binary static, so one package runs on every release of every
    # distribution that takes its format.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X $module/internal/version.version=$VERSION" \
        -o "$out/stage/$arch/$program" "./cmd/$program"
done

# FreeBSD keeps what packages install under /usr/local, so these programs are built with
# FreeBSD's folders as their defaults (see config/paths.go).
for arch in amd64 arm64; do
    mkdir -p "$out/stage/freebsd-$arch"
    CGO_ENABLED=0 GOOS=freebsd GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X $module/internal/version.version=$VERSION \
            -X $module/config.defaultConfigDir=/usr/local/etc/splicefs/client \
            -X $module/config.defaultCacheDir=/var/cache/splicefs" \
        -o "$out/stage/freebsd-$arch/$program" "./cmd/$program"
done

# The unit in the repository is written for a manual install under /usr/local.
sed 's|/usr/local/bin/|/usr/bin/|' "packaging/systemd/$program.service" > "$out/stage/$program.service"
