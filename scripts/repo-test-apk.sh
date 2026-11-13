#!/bin/sh
# Uses the Alpine repository the way a user would, with signature checking on: adds the
# public key and the repository, installs and removes the package, then proves signatures
# are enforced by removing the key. It runs inside a throwaway Alpine container
# (make repo-test) with the repository at /repo and the public keys at /keys.
set -eu

. scripts/package-lib.sh
key=/etc/apk/keys/splicefs.rsa.pub

install -m 644 /keys/splicefs.rsa.pub "$key"
mkdir -p /etc/apk/repositories.d
echo /repo/alpine > /etc/apk/repositories.d/splicefs.list

# The exact version is named because the repository may hold older builds.
apk add --update-cache "$name=$apkver-r0" >/tmp/add.log 2>&1 || {
    cat /tmp/add.log
    fail "install from the repository failed"
}
"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
apk del -q "$name" || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"

# Without the key the same repository must be refused.
rm "$key"
rm -rf /var/cache/apk/*
if apk add --update-cache "$name=$apkver-r0" >/tmp/bad.log 2>&1; then
    cat /tmp/bad.log
    fail "apk accepted a repository whose key is not trusted"
fi
grep -Eqi 'untrusted|signature' /tmp/bad.log || {
    cat /tmp/bad.log
    fail "apk failed, but not with a trust error"
}
echo "untrusted repository refused: $(grep -Eim1 'untrusted|signature' /tmp/bad.log)"

. /etc/os-release
echo "repository test passed on $PRETTY_NAME"
