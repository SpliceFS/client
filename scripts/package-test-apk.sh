#!/bin/sh
# Installs the Alpine package for this CPU type, checks what the package promises, then
# removes it and checks what is left. It runs inside a throwaway Alpine container
# (make package-test).
set -eu

dir="${1:?usage: package-test-apk.sh PACKAGE_DIR}"
# shellcheck source=package-lib.sh
. scripts/package-lib.sh
init="/etc/init.d/$program"
apk="$dir/alpine/$(apk --print-arch)/$name-$apkver-r0.apk"
[ -f "$apk" ] || fail "$apk does not exist; run make package first"

# The build key is a throwaway, so the signature cannot be checked here.
apk add -q --allow-untrusted "$apk" || fail "install failed"
apk info -e "$name" >/dev/null || fail "package is not installed"

"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
[ -x "$init" ] || fail "service script is missing or not executable"
[ -d "$confdir" ] || fail "$confdir is missing"
for folder in $confsubdirs; do
    [ -d "$confdir/$folder" ] || fail "$confdir/$folder is missing"
    contents="$(ls -A "$confdir/$folder")"
    [ -z "$contents" ] || fail "$confdir/$folder must be empty after an install"
done
# The package ships the folders and nothing else: any file there would be one an
# upgrade could overwrite.
# ls -A is used because it lists hidden entries too; the folder holds no names that need care.
# shellcheck disable=SC2012
found="$(ls -A "$confdir" | sort | tr '\n' ' ')"
expected="$(echo "$confsubdirs" | sort | tr '\n' ' ')"
[ "$found" = "$expected" ] || fail "$confdir holds something other than its empty folders"
kept="$confdir/$(echo "$confsubdirs" | head -n 1)/local.yaml"
echo "$links" | while read -r link _; do
    [ -n "$link" ] || continue
    [ -x "$link" ] || fail "$link does not lead to a program"
done
# Checks that only this package needs.
if [ -f packaging/package-test-extra.sh ]; then . packaging/package-test-extra.sh; fi
user="$(sed -n 's/^command_user="\([^:"]*\).*/\1/p' "$init")"
if [ -n "$user" ]; then
    grep -q "^$user:" /etc/passwd || fail "user $user was not created"
fi

touch "$kept"
apk del -q "$name" || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"
[ -f "$kept" ] || fail "remove must keep the configuration"

# shellcheck source=/dev/null
. /etc/os-release
# PRETTY_NAME comes from /etc/os-release.
# shellcheck disable=SC2154
echo "package test passed on $PRETTY_NAME: $(basename "$apk")"
