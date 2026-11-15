#!/bin/sh
# Wraps the compiled program into rpm packages, one per CPU type, with rpmbuild. It runs
# inside a Rocky Linux container (make package) after package-binaries.sh.
set -eu

out="${1:?usage: package-rpm.sh OUTPUT_DIR}"
# shellcheck source=package-lib.sh
. scripts/package-lib.sh

dnf install -y -q rpm-build >/dev/null

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
        --define "stage $out/stage/$arch" \
        --define "unit $out/stage/$program.service" \
        --define "src $PWD" \
        "packaging/rpm/$name.spec"
    cp "$top/RPMS/$rpmarch/$name-$rpmver-1.$rpmarch.rpm" "$out/"
    echo "built $name-$rpmver-1.$rpmarch.rpm"
done
