#!/bin/sh
# Installs the rpm package for this CPU type, checks what the package promises, then
# removes it and checks what is left. It runs inside a throwaway container of any rpm based
# system (make package-test): Rocky Linux, Amazon Linux, openSUSE.
set -eu

dir="${1:?usage: package-test-rpm.sh PACKAGE_DIR}"
. scripts/package-lib.sh
unit="/usr/lib/systemd/system/$program.service"
rpm="$dir/$name-$rpmver-1.$(uname -m).rpm"
[ -f "$rpm" ] || fail "$rpm does not exist; run make package first"

# rpm alone cannot fetch dependencies; the system's package manager does when one is
# missing. The build key is a throwaway, so zypper must be told to accept an unsigned file.
install_missing() {
    if command -v dnf >/dev/null 2>&1; then
        dnf install -y -q "$1" >/dev/null
    else
        zypper --non-interactive install --allow-unsigned-rpm "$1" >/dev/null
    fi
}
if ! rpm -i "$rpm" >/dev/null 2>&1; then
    install_missing "$rpm"
fi
rpm -q "$name" >/dev/null || fail "package is not installed"

"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
[ -f "$unit" ] || fail "service unit is missing"
if grep -q /usr/local/bin "$unit"; then fail "service unit points at /usr/local/bin"; fi
[ -d "$confdir" ] || fail "$confdir is missing"
for folder in $confsubdirs; do
    [ -d "$confdir/$folder" ] || fail "$confdir/$folder is missing"
    [ -z "$(ls -A "$confdir/$folder")" ] || fail "$confdir/$folder must be empty after an install"
done
# The package ships the folders and nothing else: any file there would be one an
# upgrade could overwrite.
[ "$(ls -A "$confdir" | sort | tr '\n' ' ')" = "$(echo "$confsubdirs" | sort | tr '\n' ' ')" ] ||
    fail "$confdir holds something other than its empty folders"
kept="$confdir/$(echo "$confsubdirs" | head -n 1)/local.yaml"
echo "$links" | while read -r link target; do
    [ -n "$link" ] || continue
    [ -x "$link" ] || fail "$link does not lead to a program"
done
# Checks that only this package needs.
if [ -f packaging/package-test-extra.sh ]; then . packaging/package-test-extra.sh; fi
user="$(sed -n 's/^User=//p' "$unit")"
if [ -n "$user" ]; then
    getent passwd "$user" >/dev/null || fail "user $user was not created"
fi

rpm -U --replacepkgs "$rpm" >/dev/null || fail "installing over itself failed"

touch "$kept"
rpm -e "$name" >/dev/null 2>&1 || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"
[ -f "$kept" ] || fail "remove must keep the configuration"

. /etc/os-release
echo "package test passed on $PRETTY_NAME: $(basename "$rpm")"
