#!/bin/sh
# Wraps the compiled programs of OUTPUT_DIR/bin (scripts/build-all.sh) into Debian packages,
# one per CPU type, with dpkg-deb. It runs in the Go image (make package-deb) and compiles
# nothing.
set -eu

out="${1:?usage: package-deb.sh OUTPUT_DIR}"
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

unit="$(mktemp)"
trap 'rm -f "$unit"' EXIT
package_unit "$unit"

for arch in amd64 arm64; do
    root="$(mktemp -d)"
    chmod 755 "$root"
    install -d "$root/DEBIAN" "$root$confdir"
    for folder in $confsubdirs; do install -d "$root$confdir/$folder"; done
    echo "$links" | while read -r link target; do
        [ -n "$link" ] || continue
        install -d "$root$(dirname "$link")"
        ln -s "$target" "$root$link"
    done
    for each in $programs; do
        install -D -m 755 "$out/bin/linux-$arch/$each" "$root/usr/bin/$each"
    done
    install -D -m 644 "$unit" "$root/usr/lib/systemd/system/$program.service"
    install -D -m 644 LICENSE "$root/usr/share/doc/$name/copyright"
    install -m 644 README.md CHANGELOG.md docs/configuration.md "$root/usr/share/doc/$name/"

    size="$(du -sk "$root" | cut -f1)"
    sed -e "s/@VERSION@/$debver/" -e "s/@ARCH@/$arch/" -e "s/@SIZE@/$size/" packaging/deb/control \
        > "$root/DEBIAN/control"
    for script in postinst prerm postrm; do
        install -m 755 "packaging/deb/$script" "$root/DEBIAN/$script"
    done

    dpkg-deb --root-owner-group --build "$root" "$out/${name}_${debver}_${arch}.deb" >/dev/null
    echo "built ${name}_${debver}_${arch}.deb"
done
