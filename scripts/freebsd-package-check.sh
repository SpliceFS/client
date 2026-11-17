#!/bin/sh
# Proves that a FreeBSD package is built for the CPU type it is named for: its manifest names
# that ABI, and its program is a program of that CPU type. The machine that builds the packages
# has one CPU type, and pkg create makes the package of the other one by being told its ABI.
# That package is never installed, so this is the only proof that it holds what it claims. It
# runs on the host or in the Go container (scripts/package-freebsd.sh) and needs tar and od.
#
#   freebsd-package-check.sh <package file> <FreeBSD major version> <amd64|aarch64> <program>
set -eu

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

if [ "$#" -ne 4 ]; then
    echo "usage: freebsd-package-check.sh PACKAGE MAJOR amd64|aarch64 PROGRAM" >&2
    exit 2
fi
package="$1"
major="$2"
arch="$3"
program="$4"

# The machine type of a program is the two bytes at offset 18 of its ELF header, little endian:
# 0x3e is x86-64 and 0xb7 is AArch64.
case "$arch" in
    amd64) machine_type=3e00 ;;
    aarch64) machine_type=b700 ;;
    *) echo "usage: freebsd-package-check.sh PACKAGE MAJOR amd64|aarch64 PROGRAM" >&2; exit 2 ;;
esac
abi="FreeBSD:$major:$arch"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tar -xf "$package" -C "$work" || fail "cannot unpack $package"

# The manifest is JSON, which may or may not have spaces around the colon.
grep -Eq "\"abi\" *: *\"$abi\"" "$work/+MANIFEST" || fail "the manifest of $package does not say $abi"
file="$work/usr/local/bin/$program"
[ -f "$file" ] || fail "$package holds no usr/local/bin/$program"
found="$(od -An -tx1 -j18 -N2 "$file" | tr -d ' \n')"
[ "$found" = "$machine_type" ] ||
    fail "the program in $package has machine type $found, not $machine_type, which is $arch"
echo "ok   $package: $abi, and the program is for $arch"
