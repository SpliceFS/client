#!/bin/sh
# Wraps the compiled programs of OUTPUT_DIR/bin (scripts/build-all.sh) into rpm packages, one
# per CPU type, with rpmbuild. It runs in the rpm-build image (make package-rpm) and compiles
# nothing.
set -eu

out="${1:?usage: package-rpm.sh OUTPUT_DIR}"
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

unit="$(mktemp)"
trap 'rm -f "$unit"' EXIT
package_unit "$unit"

for arch in amd64 arm64; do
    case "$arch" in
        amd64) rpmarch=x86_64 ;;
        arm64) rpmarch=aarch64 ;;
        *) fail "no rpm CPU type for $arch" ;;
    esac
    top="$(mktemp -d)"
    rpmbuild -bb --quiet --target "$rpmarch" \
        --define "_topdir $top" \
        --define "pkg_version $rpmver" \
        --define "stage $out/bin/linux-$arch" \
        --define "unit $unit" \
        --define "src $PWD" \
        "packaging/rpm/$name.spec"
    cp "$top/RPMS/$rpmarch/$name-$rpmver-1.$rpmarch.rpm" "$out/"
    echo "built $name-$rpmver-1.$rpmarch.rpm"
done
