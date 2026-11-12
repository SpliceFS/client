#!/bin/sh
# Installs the newest Debian package for this CPU type from the directory given as the
# first argument, checks what the package promises, then removes and purges it and checks
# what is left. It runs inside a throwaway container (make package-test).
set -eu

dir="${1:?usage: package-test.sh PACKAGE_DIR}"
: "${VERSION:?VERSION is not set}"

name="$(sed -n 's/^Package: //p' packaging/deb/control)"
program="$(basename cmd/*)"
confdir="/etc/splicefs/${name#splicefs-}"
unit="/usr/lib/systemd/system/$program.service"
deb="$(ls -t "$dir/${name}"_*_"$(dpkg --print-architecture)".deb | head -n 1)"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

dpkg-deb --info "$deb"
dpkg-deb --contents "$deb"

# dpkg alone cannot fetch dependencies; apt finishes the install when one is missing.
if ! dpkg -i "$deb" >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y -qq -f >/dev/null
fi
dpkg -s "$name" | grep -q '^Status: install ok installed' || fail "package is not installed"

"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
[ -f "$unit" ] || fail "service unit is missing"
if grep -q /usr/local/bin "$unit"; then fail "service unit points at /usr/local/bin"; fi
[ -d "$confdir" ] || fail "$confdir is missing"
[ -z "$(ls -A "$confdir")" ] || fail "$confdir must be empty after an install"
user="$(sed -n 's/^User=//p' "$unit")"
if [ -n "$user" ]; then
    getent passwd "$user" >/dev/null || fail "user $user was not created"
fi

dpkg -i "$deb" >/dev/null || fail "installing over itself failed"

touch "$confdir/local.yaml"
dpkg -r "$name" >/dev/null 2>&1 || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"
[ -f "$confdir/local.yaml" ] || fail "remove must keep the configuration"
dpkg -P "$name" >/dev/null 2>&1 || fail "purge failed"
[ ! -e "$confdir" ] || fail "purge must delete the configuration"

echo "package test passed: $(basename "$deb")"
