#!/bin/sh
# Builds the Debian packages of this program, one per CPU type, into the directory given
# as the first argument. It runs inside the build container (make package), which has Go
# and dpkg-deb. The version comes from the VERSION environment variable.
set -eu

out="${1:?usage: package.sh OUTPUT_DIR}"
: "${VERSION:?VERSION is not set}"

control=packaging/deb/control
name="$(sed -n 's/^Package: //p' "$control")"
program="$(basename cmd/*)"
module="$(go list -m)"
confdir="/etc/splicefs/${name#splicefs-}"

# A Debian version starts with a digit and has at most one hyphen-like separator:
# v1.2.3-4-gabc becomes 1.2.3+4.gabc, and a build without a release tag becomes
# 0.0.0+<commit>.
case "$VERSION" in
    v[0-9]*) version="${VERSION#v}" ;;
    *) version="0.0.0-$VERSION" ;;
esac
version="$(printf '%s' "$version" | sed -e 's/-/+/' -e 's/-/./g')"

for arch in amd64 arm64; do
    root="$(mktemp -d)"
    chmod 755 "$root"
    install -d "$root/DEBIAN" "$root/usr/bin" "$root/usr/lib/systemd/system" \
        "$root$confdir" "$root/usr/share/doc/$name"

    # CGO off makes the binary static, so one package runs on every release of every
    # distribution that takes this package format.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X $module/internal/version.version=$VERSION" \
        -o "$root/usr/bin/$program" "./cmd/$program"

    # The unit in the repository is written for a manual install under /usr/local.
    sed 's|/usr/local/bin/|/usr/bin/|' "packaging/systemd/$program.service" \
        > "$root/usr/lib/systemd/system/$program.service"
    chmod 644 "$root/usr/lib/systemd/system/$program.service"

    install -m 644 LICENSE "$root/usr/share/doc/$name/copyright"
    install -m 644 README.md CHANGELOG.md docs/configuration.md "$root/usr/share/doc/$name/"

    size="$(du -sk "$root" | cut -f1)"
    sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" -e "s/@SIZE@/$size/" "$control" \
        > "$root/DEBIAN/control"
    for script in postinst prerm postrm; do
        install -m 755 "packaging/deb/$script" "$root/DEBIAN/$script"
    done

    dpkg-deb --root-owner-group --build "$root" "$out/${name}_${version}_${arch}.deb" >/dev/null
    echo "built ${name}_${version}_${arch}.deb"
done
