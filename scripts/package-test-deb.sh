#!/bin/sh
# Installs the Debian package for this CPU type, checks what the package promises, then
# removes and purges it and checks what is left. It runs inside a throwaway Debian or
# Ubuntu container (make package-test).
set -eu

dir="${1:?usage: package-test-deb.sh PACKAGE_DIR}"
. scripts/package-lib.sh
unit="/usr/lib/systemd/system/$program.service"
deb="$dir/${name}_${debver}_$(dpkg --print-architecture).deb"
[ -f "$deb" ] || fail "$deb does not exist; run make package first"

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

. /etc/os-release
echo "package test passed on $PRETTY_NAME: $(basename "$deb")"
