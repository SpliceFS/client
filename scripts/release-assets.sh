#!/bin/sh
# Collects the files of THIS version for the GitHub release into dist/release: the two deb
# files, the two signed rpm files, the two apk files, SHA256SUMS and its detached OpenPGP
# signature SHA256SUMS.asc. Runs on the host after make package and make repo (make
# release-assets). Variables: VERSION and PACKAGE_DATE as for the build; SIGNING_KEY_FILE
# (required, armored secret signing key, no passphrase); DIST_DIR (default dist); KEYS_DIR
# (default DIST_DIR/keys, must hold the public key splicefs.asc); RELEASE_DIR (default
# DIST_DIR/release). The key is never printed.
# shellcheck disable=SC2154 # name and the versions come from package-lib.sh
set -eu

: "${SIGNING_KEY_FILE:?SIGNING_KEY_FILE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
# package-lib.sh reads VERSION and PACKAGE_DATE and says what the files are called.
# shellcheck disable=SC1091 # the sourced file is checked on its own
. scripts/package-lib.sh

dist="${DIST_DIR:-$root/dist}"
keys="${KEYS_DIR:-$dist/keys}"
out="${RELEASE_DIR:-$dist/release}"
[ -f "$SIGNING_KEY_FILE" ] && [ -r "$SIGNING_KEY_FILE" ] ||
    fail "SIGNING_KEY_FILE is not a readable file"
[ -f "$keys/splicefs.asc" ] || fail "missing $keys/splicefs.asc"
command -v gpg >/dev/null 2>&1 || fail "gpg is not on the PATH"
if command -v sha256sum >/dev/null 2>&1; then
    sums() { sha256sum -- "$@"; }
else
    sums() { shasum -a 256 -- "$@"; }
fi

mkdir -p "$out"
rm -f "$out"/*

# Only the files of this version, so older versions kept in the repositories are not
# released again. The rpm files come from the repository, where createrepo signed them.
# Both apk files have the same name, so the CPU type is added to tell them apart.
for arch in amd64 arm64; do
    f="$dist/${name}_${debver}_$arch.deb"
    [ -f "$f" ] || fail "missing $f, run make package first"
    cp "$f" "$out/"
done
for arch in x86_64 aarch64; do
    f="$dist/repo/rpm/$arch/$name-$rpmver-1.$arch.rpm"
    [ -f "$f" ] || fail "missing $f, run make package and make repo first"
    cp "$f" "$out/"
    f="$dist/alpine/$arch/$name-$apkver-r0.apk"
    [ -f "$f" ] || fail "missing $f, run make package first"
    cp "$f" "$out/$name-$apkver-r0.$arch.apk"
done

# Sorted in the C locale so the file is the same on every system.
# The names are ours and hold no spaces, so the list is split on purpose.
# shellcheck disable=SC2046
(cd "$out" && sums $(LC_ALL=C ls) > SHA256SUMS)

# Two homes: the signing home holds the secret key and the verifying home only the public
# key, so the proof cannot be satisfied by the key that made the signature. Both are removed
# on every exit path. gpg output goes to a file and is shown only on failure.
sign_home="$(mktemp -d)"
verify_home="$(mktemp -d)"
log="$(mktemp)"
cleanup() { rm -rf "$sign_home" "$verify_home" "$log"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
chmod 700 "$sign_home" "$verify_home"

GNUPGHOME="$sign_home" gpg --batch --quiet --import "$SIGNING_KEY_FILE" >"$log" 2>&1 ||
    fail "could not import the signing key"
if ! GNUPGHOME="$sign_home" gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
    --armor --detach-sign --output "$out/SHA256SUMS.asc" "$out/SHA256SUMS" >"$log" 2>&1; then
    sed 's/^/  /' "$log" >&2
    fail "signing failed"
fi

GNUPGHOME="$verify_home" gpg --batch --quiet --import "$keys/splicefs.asc" >"$log" 2>&1 ||
    fail "could not import $keys/splicefs.asc"
want="$(GNUPGHOME="$verify_home" gpg --batch --with-colons --list-keys 2>/dev/null |
    awk -F: '$1 == "pub" { w = 1 } $1 == "fpr" && w { print $10; exit }')"
[ -n "$want" ] || fail "no fingerprint in $keys/splicefs.asc"
# VALIDSIG field 11 of the status line is the primary key fingerprint, also for a subkey.
GNUPGHOME="$verify_home" gpg --batch --status-fd 1 --verify "$out/SHA256SUMS.asc" \
    "$out/SHA256SUMS" >"$log" 2>/dev/null ||
    fail "SHA256SUMS.asc does not verify against $keys/splicefs.asc"
got="$(awk '$2 == "VALIDSIG" { print $12 }' "$log")"
[ "$got" = "$want" ] || fail "SHA256SUMS.asc was not made by the key $want"
chmod 644 "$out"/*
echo "release files in $out, SHA256SUMS signed and verified against key $want"
