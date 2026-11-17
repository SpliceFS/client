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
check v1.2.3-rc.1 1.2.3~rc.1 1.2.3_rc1 1.2.3.rc1
check v1.2.3-beta.2 1.2.3~beta.2 1.2.3_beta2 1.2.3.beta2
check v1.2.3-alpha 1.2.3~alpha 1.2.3_alpha 1.2.3.alpha
check v1.2.3-dirty 1.2.3+dirty 1.2.3_git20261112 1.2.3.20261112
check v1.2.3-rc.1-dirty 1.2.3~rc.1+dirty 1.2.3_rc1_git20261112 1.2.3.rc1.20261112
refuse v1.2.3-foo.1 v1.2.3-foo.1
refuse v1.2 v1.2
refuse v1.2.3-rc.1.2 v1.2.3-rc.1.2
# What git describe prints between releases and without tags is not a version any more; a
# build that gets one has not read the file VERSION.
refuse v1.2.3-4-gabc1234 v1.2.3-4-gabc1234
refuse abc1234 abc1234
refuse abc1234-dirty abc1234-dirty
refuse dev dev
refuse 1.2.3 1.2.3

# The unit in the repository is written for a manual install under /usr/local; the unit a
# package ships has the program in /usr/bin, and nothing else is lost.
program="$(VERSION=v1.2.3 PACKAGE_DATE=20261112 sh -c '. scripts/package-lib.sh; echo "$program"')"
got="$(VERSION=v1.2.3 PACKAGE_DATE=20261112 sh -c '. scripts/package-lib.sh; package_unit /dev/stdout' 2>&1)" || true
got_lines="$(printf '%s\n' "$got" | wc -l)"
repo_lines="$(wc -l < "packaging/systemd/$program.service")"
if printf '%s\n' "$got" | grep -qx "ExecStart=/usr/bin/$program" &&
    ! printf '%s\n' "$got" | grep -q '/usr/local/' && [ "$got_lines" -eq "$repo_lines" ]; then
    echo "ok   the packaged unit has the program in /usr/bin"
else
    printf 'FAIL the packaged unit has the wrong program path or lost lines: %s\n' "$got"
    failed=$((failed + 1))
fi

# ONLY limits a run to one format or one image, for every script that walks packaging/test-systems.
# $1 ONLY, $2 format, $3 image, $4 selected (yes or no).
selects() {
    if ONLY="$1" VERSION=v1.2.3 PACKAGE_DATE=20261112 sh -c \
        '. scripts/package-lib.sh; system_selected "$1" "$2"' sh "$2" "$3"; then got=yes; else got=no; fi
    if [ "$got" = "$4" ]; then
        printf 'ok   ONLY="%s" %s %s: %s\n' "$1" "$2" "$3" "$got"
    else
        printf 'FAIL ONLY="%s" %s %s: want %s got %s\n' "$1" "$2" "$3" "$4" "$got"
        failed=$((failed + 1))
    fi
}

selects "" deb ghcr.io/splicefs/server-debian-12:v1.0 yes
selects deb deb ghcr.io/splicefs/server-debian-12:v1.0 yes
selects rpm deb ghcr.io/splicefs/server-debian-12:v1.0 no
selects ghcr.io/splicefs/server-debian-12:v1.0 deb ghcr.io/splicefs/server-debian-12:v1.0 yes
selects ghcr.io/splicefs/server-debian-12:v1.0 deb ghcr.io/splicefs/server-ubuntu-24.04:v1.0 no
# A part of a name is not a name: the filter says which format or which image, not a pattern.
selects deb rpm ghcr.io/splicefs/server-amazonlinux-2023:v1.0 no
selects server-rocky-9 rpm ghcr.io/splicefs/server-rocky-9:v1.0 no

# A system is started the way the images README boots a server image, and the test waits for
# its service manager the way that manager reports it. A stand-in for docker records what it
# is asked and answers as a system in the state given by the variables of each case.
fake="$(mktemp -d)"
trap 'rm -rf "$fake"' EXIT
cat > "$fake/docker" <<'DOCKER'
#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1 $3" in
    "exec systemctl") echo "$FAKE_SYSTEMD" ;;
    "exec rc-status") echo "$FAKE_RUNLEVEL" ;;
    "inspect "*) echo "$FAKE_RUNNING" ;;
    *) ;;
esac
DOCKER
chmod +x "$fake/docker"

# $1 what is passed to boot_system, $2 the command line docker must get.
boots() {
    : > "$fake/log"
    # $1 is split on purpose: it holds several arguments.
    # shellcheck disable=SC2086
    got="$(PATH="$fake:$PATH" FAKE_LOG="$fake/log" VERSION=v1.2.3 PACKAGE_DATE=20261112 sh -c \
        '. scripts/package-lib.sh; boot_system "$@"' sh $1 2>&1 && cat "$fake/log")" || true
    if [ "$got" = "$2" ]; then
        printf 'ok   boot_system %s\n' "$1"
    else
        printf 'FAIL boot_system %s: want "%s" got "%s"\n' "$1" "$2" "$got"
        failed=$((failed + 1))
    fi
}

# $1 format, $2 state of systemd, $3 runlevel of OpenRC, $4 whether the container runs,
# $5 the status wait_booted must return. Each case is ready at once or never, so none waits.
waits() {
    : > "$fake/log"
    got=0
    PATH="$fake:$PATH" FAKE_LOG="$fake/log" FAKE_SYSTEMD="$2" FAKE_RUNLEVEL="$3" FAKE_RUNNING="$4" \
        VERSION=v1.2.3 PACKAGE_DATE=20261112 sh -c '. scripts/package-lib.sh; wait_booted "$1" box' sh "$1" >/dev/null 2>&1 || got=$?
    if [ "$got" -eq "$5" ]; then
        printf 'ok   wait_booted %-4s systemd=%-8s runlevel=%-8s running=%-5s status %s\n' "$1" "$2" "$3" "$4" "$got"
    else
        printf 'FAIL wait_booted %-4s systemd=%-8s runlevel=%-8s running=%-5s want status %s got %s\n' \
            "$1" "$2" "$3" "$4" "$5" "$got"
        failed=$((failed + 1))
    fi
}

boots "--name box ghcr.io/splicefs/server-debian-12:v1.0" "run -d --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock --tmpfs /tmp --name box ghcr.io/splicefs/server-debian-12:v1.0"
waits deb running "" true 0
waits rpm degraded "" true 0
waits apk "" default true 0
# A format is waited for with its own service manager: the other one's answer does not count.
waits apk running "" false 1
waits deb "" default false 1
# A container that has stopped will never be ready, so the wait ends at once.
waits deb starting "" false 1
waits apk "" boot false 1

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "package versions: all mappings as expected"
