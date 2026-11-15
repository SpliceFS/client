#!/bin/sh
# Table test of how scripts/package-lib.sh turns VERSION into each package version. Runs on
# the host, no Docker (make package-lib-test, part of make check). Whether the results also
# sort right in each package manager is checked in containers by scripts/package-order-test.sh.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
failed=0

# $1 VERSION, $2 deb and rpm version, $3 Alpine version, $4 FreeBSD version (PACKAGE_DATE is
# 20261112).
check() {
    got="$(VERSION="$1" PACKAGE_DATE=20261112 sh -c \
        '. scripts/package-lib.sh; echo "$debver $rpmver $apkver $pkgver"' 2>&1)" || true
    want="$2 $2 $3 $4"
    if [ "$got" = "$want" ]; then
        printf 'ok   %-28s deb/rpm %-26s apk %-24s pkg %s\n' "$1" "$2" "$3" "$4"
    else
        printf 'FAIL %-28s want "%s" got "%s"\n' "$1" "$want" "$got"
        failed=$((failed + 1))
    fi
}

# $1 VERSION that must be refused, $2 text the message must contain.
refuse() {
    if got="$(VERSION="$1" PACKAGE_DATE=20261112 sh -c '. scripts/package-lib.sh' 2>&1)"; then
        printf 'FAIL %-28s was accepted\n' "$1"
        failed=$((failed + 1))
    elif printf '%s' "$got" | grep -q -- "$2"; then
        printf 'ok   %-28s refused: %s\n' "$1" "$got"
    else
        printf 'FAIL %-28s refused with the wrong message: %s\n' "$1" "$got"
        failed=$((failed + 1))
    fi
}

check v1.2.3 1.2.3 1.2.3 1.2.3
check v1.2.3-4-gabc1234 1.2.3+4.gabc1234 1.2.3_git20261112 1.2.3.20261112
check v1.2.3-rc.1 1.2.3~rc.1 1.2.3_rc1 1.2.3.rc1
check v1.2.3-beta.2 1.2.3~beta.2 1.2.3_beta2 1.2.3.beta2
check v1.2.3-alpha 1.2.3~alpha 1.2.3_alpha 1.2.3.alpha
check v1.2.3-rc.1-4-gabc1234 1.2.3~rc.1+4.gabc1234 1.2.3_rc1_git20261112 1.2.3.rc1.20261112
check abc1234 0.0.0+abc1234 0.0.0_git20261112 0.0.0.20261112
check v1.2.3-dirty 1.2.3+dirty 1.2.3_git20261112 1.2.3.20261112
check v1.2.3-4-gabc1234-dirty 1.2.3+4.gabc1234.dirty 1.2.3_git20261112 1.2.3.20261112
check v1.2.3-rc.1-dirty 1.2.3~rc.1+dirty 1.2.3_rc1_git20261112 1.2.3.rc1.20261112
check abc1234-dirty 0.0.0+abc1234.dirty 0.0.0_git20261112 0.0.0.20261112
refuse v1.2.3-foo.1 v1.2.3-foo.1
refuse v1.2 v1.2
refuse v1.2.3-rc.1.2 v1.2.3-rc.1.2

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "package versions: all mappings as expected"
