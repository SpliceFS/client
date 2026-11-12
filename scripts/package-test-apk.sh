#!/bin/sh
# Installs the Alpine package for this CPU type, checks what the package promises, then
# removes it and checks what is left. It runs inside a throwaway Alpine container
# (make package-test).
set -eu

dir="${1:?usage: package-test-apk.sh PACKAGE_DIR}"
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
    [ -z "$(ls -A "$confdir/$folder")" ] || fail "$confdir/$folder must be empty after an install"
done
# The package ships the folders and nothing else: any file there would be one an
# upgrade could overwrite.
[ "$(ls -A "$confdir" | sort | tr '\n' ' ')" = "$(echo "$confsubdirs" | sort | tr '\n' ' ')" ] ||
    fail "$confdir holds something other than its empty folders"
kept="$confdir/$(echo "$confsubdirs" | head -n 1)/local.yaml"
user="$(sed -n 's/^command_user="\([^:"]*\).*/\1/p' "$init")"
if [ -n "$user" ]; then
    grep -q "^$user:" /etc/passwd || fail "user $user was not created"
fi

touch "$kept"
apk del -q "$name" || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"
[ -f "$kept" ] || fail "remove must keep the configuration"

. /etc/os-release
echo "package test passed on $PRETTY_NAME: $(basename "$apk")"
