#!/bin/sh
# Uses the apt repository the way a user would, with signature checking on: adds the public
# key and a source entry, installs and removes the package, then proves signatures are
# enforced by pointing apt at a damaged copy. It runs inside a throwaway Debian or Ubuntu
# container (make repo-test) with the repository at /repo and the public keys at /keys.
set -eu

. scripts/package-lib.sh
sources=/etc/apt/sources.list.d/splicefs.sources

install -d -m 755 /etc/apt/keyrings
install -m 644 /keys/splicefs.asc /etc/apt/keyrings/splicefs.asc
cat > "$sources" <<SOURCE
Types: deb
URIs: file:///repo/apt
Suites: stable
Components: main
Signed-By: /etc/apt/keyrings/splicefs.asc
SOURCE

apt-get update >/tmp/update.log 2>&1 || { cat /tmp/update.log; fail "apt-get update failed"; }
grep -q 'file:/repo/apt stable InRelease' /tmp/update.log || fail "apt did not read InRelease"
if grep -Eq 'NO_PUBKEY|BADSIG|is not signed|EXPKEYSIG' /tmp/update.log; then
    cat /tmp/update.log
    fail "apt reported a signature problem on the good repository"
fi

# The exact version is named because dist may hold older builds that apt would prefer.
apt-get install -y -qq "$name=$debver" >/dev/null
"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
apt-get remove -y -qq "$name" >/dev/null
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"

# /repo is read-only, so the damaged copy is private. Release and InRelease are both
# changed, otherwise apt would fall back to the intact one of the two.
cp -r /repo /tmp/bad-repo
sed -i 's/^Suite: stable/Suite: stablf/' /tmp/bad-repo/apt/dists/stable/Release \
    /tmp/bad-repo/apt/dists/stable/InRelease
sed -i 's#file:///repo/apt#file:///tmp/bad-repo/apt#' "$sources"
if apt-get update >/tmp/bad.log 2>&1; then
    cat /tmp/bad.log
    fail "apt-get update accepted a repository with a broken signature"
fi
grep -Eq 'BADSIG|not signed|signatures were invalid|NO_PUBKEY' /tmp/bad.log || {
    cat /tmp/bad.log
    fail "apt-get update failed, but not with a signature error"
}
echo "damaged repository refused: $(grep -Em1 'BADSIG|not signed|invalid|NO_PUBKEY' /tmp/bad.log)"

. /etc/os-release
echo "repository test passed on $PRETTY_NAME"
