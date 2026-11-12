#!/bin/sh
# Wraps the compiled program into Debian packages, one per CPU type, with dpkg-deb. It runs
# inside a Debian container (make package) after package-binaries.sh.
set -eu

out="${1:?usage: package-deb.sh OUTPUT_DIR}"
. scripts/package-lib.sh

for arch in amd64 arm64; do
    root="$(mktemp -d)"
    chmod 755 "$root"
    install -d "$root/DEBIAN" "$root$confdir"
    install -D -m 755 "$out/stage/$arch/$program" "$root/usr/bin/$program"
    install -D -m 644 "$out/stage/$program.service" "$root/usr/lib/systemd/system/$program.service"
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
