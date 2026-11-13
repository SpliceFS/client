#!/bin/sh
# Checks that the versions package-lib.sh makes sort the way a release process needs in each
# package manager: dpkg in Debian, rpm in Rocky Linux, apk in Alpine, each in its own
# container. Runs on the host (make package-order-test, part of make package-test, so CI and
# the release job run it). Every version in the list below must compare as older than the
# next one, and apk must accept every Alpine version as valid.
# Environment: DEB_IMAGE, RPM_IMAGE, APK_IMAGE name the images; PACKAGE_DATE as for the build.
# Inside a container the script is called as: package-order-test.sh inside FORMAT VERSION...
set -eu

if [ "${1:-}" = inside ]; then
    format="$2"
    shift 2
    prev=""
    status=0
    for v in "$@"; do
        # apk version -c prints a version it cannot parse and exits 1; a valid one prints nothing.
        if [ "$format" = apk ] && ! apk version -c "$v" >/dev/null; then
            echo "FAIL apk does not accept $v"
            status=1
        fi
        if [ -n "$prev" ]; then
            case "$format" in
                deb)
                    if dpkg --compare-versions "$prev" lt "$v"; then r='<'; else r=other; fi ;;
                rpm)
                    r="$(rpm --eval "%{lua:print(rpm.vercmp('$prev', '$v'))}")"
                    if [ "$r" = -1 ]; then r='<'; fi ;;
                apk)
                    r="$(apk version -t "$prev" "$v")" ;;
                *)
                    echo "FAIL unknown format $format" >&2
                    exit 1 ;;
            esac
            if [ "$r" = '<' ]; then
                echo "ok   $format $prev < $v"
            else
                echo "FAIL $format $prev is not older than $v (result $r)"
                status=1
            fi
        fi
        prev="$v"
    done
    exit "$status"
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
export PACKAGE_DATE="${PACKAGE_DATE:-20261112}"

# Ascending order of what the release process produces. An untagged build (0.0.0) is older
# than every release, and each pre-release is older than its release.
versions="abc1234 v0.9.0 v1.2.3-alpha v1.2.3-beta.2 v1.2.3-rc.1 v1.2.3-rc.1-4-gabc1234
v1.2.3-rc.2 v1.2.3 v1.2.3-4-gabc1234 v1.2.4-rc.1 v1.2.4"

failed=0
for format in deb rpm apk; do
    case "$format" in
        deb) image="${DEB_IMAGE:-debian:12}" ;;
        rpm) image="${RPM_IMAGE:-rockylinux/rockylinux:9}" ;;
        apk) image="${APK_IMAGE:-alpine:3.22}" ;;
        *)
            echo "FAIL unknown format $format" >&2
            exit 1 ;;
    esac
    list=""
    for v in $versions; do
        got="$(VERSION="$v" FORMAT="$format" sh -c \
            '. scripts/package-lib.sh; eval "echo \$${FORMAT}ver"')"
        list="$list $got"
    done
    # $list is split on purpose: one argument per version, none contains a space.
    # shellcheck disable=SC2086
    docker run --rm -v "$root":/src:ro "$image" sh /src/scripts/package-order-test.sh \
        inside "$format" $list || failed=$((failed + 1))
done
[ "$failed" -eq 0 ] || { echo "ordering failed for $failed format(s)"; exit 1; }
echo "package version ordering: ok in deb, rpm and apk"
