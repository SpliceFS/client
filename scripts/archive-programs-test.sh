#!/bin/sh
# Test of scripts/archive-programs.sh on a small repository of its own: names of the archives,
# their members, owner, group, modes and times as stored in the tar headers, the bytes of the
# files, that two runs give identical archives, and the refusals. It runs on the host, needs
# no Docker and compiles nothing (make archive-programs-test, part of make unit).
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# $1 case name, $2 expected, $3 got.
check() {
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s\n  expected: %s\n  got:      %s\n' "$1" "$2" "$3"
        failed=$((failed + 1))
    fi
}

# A repository with one program name, the real libraries, and the programs of a build:
# alpha for every system, beta for Linux only, so that each archive holds what its own
# system has.
tree="$work/tree"
mkdir -p "$tree/scripts" "$tree/cmd/alpha" "$tree/packaging/deb" "$tree/packaging/plain" \
    "$tree/packaging/systemd"
: > "$tree/packaging/systemd/alpha.service"
cp "$root/scripts/archive-programs.sh" "$root/scripts/package-lib.sh" "$tree/scripts/"
printf 'Package: splicefs-demo\n' > "$tree/packaging/deb/control"
printf 'conf.d\n' > "$tree/packaging/config-folders"
printf 'how to run the programs\n' > "$tree/packaging/plain/README.txt"
printf 'the license\n' > "$tree/LICENSE"
for system in linux-amd64 linux-arm64 freebsd-amd64 freebsd-arm64 darwin-amd64 darwin-arm64; do
    mkdir -p "$tree/dist/bin/$system"
    printf 'alpha for %s\n' "$system" > "$tree/dist/bin/$system/alpha"
    chmod 700 "$tree/dist/bin/$system/alpha"
done
for system in linux-amd64 linux-arm64; do
    printf 'beta for %s\n' "$system" > "$tree/dist/bin/$system/beta"
    chmod 755 "$tree/dist/bin/$system/beta"
done

# Runs the script in the repository with the version and date of the arguments.
archive() {
    (cd "$tree" && VERSION="$1" PACKAGE_DATE="$2" sh scripts/archive-programs.sh)
}

# Prints name, mode, owner, group and time (all octal, without leading zeros) of each member of
# the gzip tar $1, read from the 512 byte headers at their fixed places. That is the same for
# every tar program, which print owners and times in their own ways and write the numbers with
# more or fewer zeros. od turns the bytes into numbers, and awk is one process for the whole
# walk, because many small processes make the test slow.
headers() {
    gzip -dc "$1" | od -An -v -tu1 | awk '
        function text(from, length_,    i, c, out) {
            out = ""
            for (i = 0; i < length_; i++) {
                c = byte[from + i]
                if (c > 32) out = out sprintf("%c", c)
            }
            return out
        }
        function number(from, length_,    digits, i, value) {
            digits = text(from, length_)
            value = 0
            for (i = 1; i <= length(digits); i++) value = value * 8 + substr(digits, i, 1)
            return value
        }
        function octal(value,    out) {
            out = ""
            do { out = (value % 8) out; value = int(value / 8) } while (value > 0)
            return out
        }
        { for (i = 1; i <= NF; i++) byte[count++] = $i }
        END {
            for (offset = 0; offset < count; offset += 512 + int((number(offset + 124, 12) + 511) / 512) * 512) {
                name = text(offset, 100)
                if (name == "") break
                print name, octal(number(offset + 100, 8)), octal(number(offset + 108, 8)), \
                    octal(number(offset + 116, 8)), octal(number(offset + 136, 12))
            }
        }'
}

archive v1.2.3 20261112 >/dev/null
dist="$tree/dist"

systems="darwin-amd64 darwin-arm64 freebsd-amd64 freebsd-arm64 linux-amd64 linux-arm64"
expected=""
for system in $systems; do expected="$expected
splicefs-demo-1.2.3-$system.tar.gz"; done
got="$(cd "$dist" && LC_ALL=C ls)"
check "one archive for each system and CPU type, named with the version without the v" \
    "bin$expected" "$got"

top=splicefs-demo-1.2.3-linux-amd64
got="$(tar -tzf "$dist/$top.tar.gz")"
check "linux holds both programs, the license and the README, sorted" \
    "$top/
$top/LICENSE
$top/README.txt
$top/alpha
$top/beta" "$got"

top=splicefs-demo-1.2.3-darwin-arm64
got="$(tar -tzf "$dist/$top.tar.gz")"
check "darwin holds only the programs it was built with" \
    "$top/
$top/LICENSE
$top/README.txt
$top/alpha" "$got"

# 15275200600 is 2026-11-12 00:00:00 UTC, the date given, in octal.
top=splicefs-demo-1.2.3-linux-amd64
got="$(headers "$dist/$top.tar.gz")"
check "owner and group 0, programs 755, other files 644, time is the date of the build" \
    "$top/ 755 0 0 15275200600
$top/LICENSE 644 0 0 15275200600
$top/README.txt 644 0 0 15275200600
$top/alpha 755 0 0 15275200600
$top/beta 755 0 0 15275200600" "$got"

mkdir "$work/extract"
tar -xzf "$dist/$top.tar.gz" -C "$work/extract"
got="$(cat "$work/extract/$top/alpha" "$work/extract/$top/beta")"
check "the programs are the built files" "alpha for linux-amd64
beta for linux-amd64" "$got"
got="$(cat "$work/extract/$top/LICENSE" "$work/extract/$top/README.txt")"
check "the license and the README are the files of the repository" "the license
how to run the programs" "$got"

# Two systems are enough for the runs below, and a run of the script costs a process for each
# step of each system.
for system in linux-arm64 freebsd-amd64 freebsd-arm64 darwin-amd64; do rm -r "$tree/dist/bin/$system"; done
rm "$dist"/*.tar.gz

# Two runs give the same bytes, also when every source file has another time in between.
archive v1.2.3 20261112 >/dev/null
(cd "$dist" && cksum -- *.tar.gz) > "$work/first"
rm "$dist"/*.tar.gz
find "$tree/dist/bin" "$tree/LICENSE" "$tree/packaging/plain" -type f -exec touch -t 202001010101 {} +
archive v1.2.3 20261112 >/dev/null
(cd "$dist" && cksum -- *.tar.gz) > "$work/second"
first="$(cat "$work/first")"
second="$(cat "$work/second")"
check "two runs give identical archives" "$first" "$second"

# 13602760400 is 2020-01-01 00:00:00 UTC.
rm "$dist"/*.tar.gz
archive v1.2.3 20200101 >/dev/null
got="$(headers "$dist/$top.tar.gz")"
check "the time follows PACKAGE_DATE" "$top/ 755 0 0 13602760400" "${got%%
*}"

# A pre-release or a build with changes names its version in full.
archive v1.2.4-rc.1-dirty 20261112 >/dev/null
got="no archive"
if [ -f "$dist/splicefs-demo-1.2.4-rc.1-dirty-darwin-arm64.tar.gz" ]; then got=present; fi
check "the pre-release version is kept in the name" present "$got"

# $1 case name, $2 text the message must hold, the rest: the command to run in the repository.
refuses() {
    name="$1"
    want="$2"
    shift 2
    status=0
    message="$(cd "$tree" && "$@" 2>&1)" || status=$?
    if [ "$status" -ne 0 ] && printf '%s' "$message" | grep -F -q -- "$want"; then
        printf 'ok   %s\n' "$name"
    else
        printf 'FAIL %s (status %s): %s\n' "$name" "$status" "$message"
        failed=$((failed + 1))
    fi
}

refuses "a PACKAGE_DATE that is not a date is refused" "PACKAGE_DATE" \
    env VERSION=v1.2.3 PACKAGE_DATE=tomorrow sh scripts/archive-programs.sh
mkdir "$tree/dist/bin/linux-riscv64"
refuses "a system without programs is refused" "linux-riscv64" \
    env VERSION=v1.2.3 PACKAGE_DATE=20261112 sh scripts/archive-programs.sh
rm -r "$tree/dist/bin"
refuses "a missing dist/bin is refused" "make build-all" \
    env VERSION=v1.2.3 PACKAGE_DATE=20261112 sh scripts/archive-programs.sh

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "archive-programs: names, members, owner, modes, times and bytes as expected"
