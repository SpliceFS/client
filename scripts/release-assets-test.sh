#!/bin/sh
# Proves release-assets.sh without a release: a private dist holds one plain program archive for
# each system, and the signing key is a throwaway key made here. Runs on the host and needs gpg
# (make release-assets-test); nothing is written outside a temporary folder.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
command -v gpg >/dev/null 2>&1 || { echo "gpg is not on the PATH" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
failed=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; failed=$((failed + 1)); }

# The signing key and the public key it is checked against, in files, as the release job has
# them. The key never leaves the temporary folder.
mkdir "$tmp/gnupg" "$tmp/keys"
chmod 700 "$tmp/gnupg"
GNUPGHOME="$tmp/gnupg" gpg --batch --pinentry-mode loopback --passphrase "" \
    --quick-generate-key "Release test key <test@example.invalid>" ed25519 sign never 2>/dev/null
GNUPGHOME="$tmp/gnupg" gpg --batch --pinentry-mode loopback --passphrase "" --armor \
    --export-secret-keys > "$tmp/keys/signing.asc"
GNUPGHOME="$tmp/gnupg" gpg --armor --export > "$tmp/keys/splicefs.asc"

# Release files for a pre-release version, from a private dist that holds one plain program
# archive for each system of its dist/bin, under the names archive-lib.sh gives this version.
export VERSION=v1.2.3-rc.1 PACKAGE_DATE=20261112
name="$(sh -c '. scripts/archive-lib.sh; echo "$name"')"
d="$tmp/dist"
mkdir -p "$d/keys"
systems="darwin-amd64 darwin-arm64 freebsd-amd64 freebsd-arm64 linux-amd64 linux-arm64"
for system in $systems; do
    mkdir -p "$d/bin/$system"
    archive="$(sh -c '. scripts/archive-lib.sh; archive_name "$1"' sh "$system")"
    echo "archive $system" > "$d/$archive"
done
# Another version in the same folder must not be collected, and neither must a file that is
# not a program archive: the packages are made and published by the package repository.
echo other > "$d/${name}-0.0.1-linux-amd64.tar.gz"
echo package > "$d/${name}_1.2.3~rc.1_amd64.deb"
cp "$tmp/keys/splicefs.asc" "$d/keys/"
if DIST_DIR="$d" SIGNING_KEY_FILE="$tmp/keys/signing.asc" \
    sh scripts/release-assets.sh >"$tmp/out" 2>&1; then
    rel="$d/release"
    count="$(find "$rel" -type f | wc -l | tr -d ' ')"
    if [ "$count" -eq 8 ]; then pass "release-assets made 8 files"; else
        fail "expected 8 files, got $count"; ls "$rel"; fi
    if command -v sha256sum >/dev/null 2>&1; then sumcheck="sha256sum -c"; else
        sumcheck="shasum -a 256 -c"; fi
    if (cd "$rel" && $sumcheck SHA256SUMS >/dev/null 2>&1); then
        pass "SHA256SUMS verifies with $sumcheck"
    else fail "SHA256SUMS does not verify"; fi
    archives="$(grep -c "^[0-9a-f]*  $name-${VERSION#v}-.*\.tar\.gz\$" "$rel/SHA256SUMS" || true)"
    listed="$(wc -l < "$rel/SHA256SUMS" | tr -d ' ')"
    if [ "$archives" -eq 6 ] && [ "$listed" -eq 6 ]; then
        pass "SHA256SUMS lists the plain program archive of each of the 6 systems and nothing else"
    else fail "expected exactly the 6 program archives in SHA256SUMS, got $archives"; fi
    mkdir "$tmp/vh"; chmod 700 "$tmp/vh"
    if GNUPGHOME="$tmp/vh" gpg --batch --quiet --import "$tmp/keys/splicefs.asc" 2>/dev/null &&
        GNUPGHOME="$tmp/vh" gpg --batch --verify "$rel/SHA256SUMS.asc" "$rel/SHA256SUMS" \
            2>/dev/null; then
        pass "SHA256SUMS.asc verifies with the public key"
    else fail "SHA256SUMS.asc does not verify"; fi
else
    fail "release-assets.sh"; sed 's/^/     /' "$tmp/out"
fi

[ "$failed" -eq 0 ] || { echo "$failed check(s) failed"; exit 1; }
echo "release-assets checks done"
