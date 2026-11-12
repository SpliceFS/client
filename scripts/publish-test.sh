#!/bin/sh
# Proves publish.sh and release-assets.sh without a server: the upload goes to local folders
# through UPLOAD_TARGET, and the signing uses the throwaway keys that make repo puts in
# dist/keys. Runs on the host (make publish-test). Needs dist/repo and dist/keys, so run
# make package repo first. Nothing here changes dist/repo: every damaged copy is private.
# A step that needs rsync --chmod prints SKIP when rsync has none (openrsync on macOS).
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
[ -d dist/repo/apt ] && [ -f dist/keys/splicefs.asc ] ||
    { echo "no dist/repo or dist/keys: run make package repo first" >&2; exit 1; }
command -v rsync >/dev/null 2>&1 || { echo "rsync is not on the PATH" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
failed=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; failed=$((failed + 1)); }
skip() { echo "SKIP $*"; }

chmod_ok=1
rsync --chmod=F644 --list-only "$tmp" >/dev/null 2>&1 || chmod_ok=0
send() { REPO_DIR="$1" UPLOAD_TARGET="$2" sh scripts/publish.sh send; }

# (a) A send to an empty folder reproduces dist/repo, with the modes the web server needs.
if [ "$chmod_ok" -eq 0 ]; then
    skip "a send to an empty folder: rsync has no --chmod"
    skip "b stale index removed, package missing locally refused: rsync has no --chmod"
    skip "c send refuses when a published package would disappear: rsync has no --chmod"
    cp -R dist/repo "$tmp/target"
else
    mkdir "$tmp/target"
    if send dist/repo "$tmp/target" >"$tmp/out" 2>&1 && diff -r dist/repo "$tmp/target" &&
        [ -z "$(find "$tmp/target" -type f ! -perm 644)" ] &&
        [ -z "$(find "$tmp/target" -type d ! -perm 755)" ]; then
        pass "a send to an empty folder gives the same tree, modes 644 and 755"
    else
        fail "a send to an empty folder"; sed 's/^/     /' "$tmp/out"
    fi

    # (b) Index files that a new build replaces go; that is the only thing a send removes.
    echo stale > "$tmp/target/rpm/x86_64/repodata/stale-primary.xml.gz"
    if send dist/repo "$tmp/target" >"$tmp/out" 2>&1 &&
        [ ! -e "$tmp/target/rpm/x86_64/repodata/stale-primary.xml.gz" ] &&
        diff -r dist/repo "$tmp/target"; then
        pass "b the stale repodata file is gone and nothing else changed"
    else
        fail "b stale repodata removed"; sed 's/^/     /' "$tmp/out"
    fi

    # A package that is published but missing here means the download step was skipped or
    # failed. Sending then would publish an index without it, so the send must refuse.
    old="apt/pool/main/old/old-package_0.0.1_amd64.deb"
    mkdir -p "$tmp/target/$(dirname "$old")"
    echo old > "$tmp/target/$old"
    if send dist/repo "$tmp/target" >"$tmp/out" 2>&1; then
        fail "b send went ahead although a published package is missing locally"
    elif [ -f "$tmp/target/$old" ] && grep -q "$old" "$tmp/out"; then
        pass "b send refused because a published package is missing locally, and it is still there"
    else
        fail "b refusal for a package missing locally"; sed 's/^/     /' "$tmp/out"
    fi
    rm "$tmp/target/$old"
    rmdir "$tmp/target/$(dirname "$old")"

    # (c) A repository that lacks a published package is refused and nothing is deleted.
    cp -R dist/repo "$tmp/copy"
    rpm="$(cd dist/repo && find rpm/x86_64 -name "*.rpm" | head -n 1)"
    rm "$tmp/copy/$rpm"
    if send "$tmp/copy" "$tmp/target" >"$tmp/out" 2>&1; then
        fail "c send must refuse when a published package would disappear"
    elif grep -q 'would delete' "$tmp/out" && [ -f "$tmp/target/$rpm" ]; then
        pass "c send refused because $rpm would disappear, and it is still published"
    else
        fail "c send refused for the wrong reason or deleted the file"; sed 's/^/     /' "$tmp/out"
    fi
fi

# (d) fetch into an empty folder reproduces what is published. Without --chmod the published
# folder is a plain copy, which is all fetch needs.
mkdir "$tmp/fetched"
if REPO_DIR="$tmp/fetched" UPLOAD_TARGET="$tmp/target" sh scripts/publish.sh fetch \
    >"$tmp/out" 2>&1 && diff -r "$tmp/target" "$tmp/fetched"; then
    pass "d fetch reproduces the published folder"
else
    fail "d fetch"; sed 's/^/     /' "$tmp/out"
fi
mkdir "$tmp/emptyremote"
if REPO_DIR="$tmp/fetched2" UPLOAD_TARGET="$tmp/emptyremote" sh scripts/publish.sh fetch \
    >"$tmp/out" 2>&1 && [ -d "$tmp/fetched2" ]; then
    pass "d fetch from an empty remote works"
else
    fail "d fetch from an empty remote"; sed 's/^/     /' "$tmp/out"
fi

# (e) Release files for a pre-release version, from a private dist that holds one stand-in
# file per package kind under the names package-lib.sh gives this version.
export VERSION=v1.2.3-rc.1 PACKAGE_DATE=20261112
name="$(VERSION="$VERSION" sh -c '. scripts/package-lib.sh; echo "$name $debver $rpmver $apkver"')"
# shellcheck disable=SC2086 # four words without spaces
set -- $name
name="$1" debver="$2" rpmver="$3" apkver="$4"
d="$tmp/dist"
mkdir -p "$d/repo/rpm/x86_64" "$d/repo/rpm/aarch64" "$d/alpine/x86_64" "$d/alpine/aarch64"
mkdir -p "$d/keys"
for arch in amd64 arm64; do echo "deb $arch" > "$d/${name}_${debver}_$arch.deb"; done
for arch in x86_64 aarch64; do
    echo "rpm $arch" > "$d/repo/rpm/$arch/$name-$rpmver-1.$arch.rpm"
    echo "apk $arch" > "$d/alpine/$arch/$name-$apkver-r0.apk"
done
# Another version in the same folders must not be collected.
echo other > "$d/${name}_0.0.1_amd64.deb"
cp dist/keys/splicefs.asc "$d/keys/"
if DIST_DIR="$d" SIGNING_KEY_FILE="$root/dist/keys/splicefs-openpgp-signing-subkey.asc" \
    sh scripts/release-assets.sh >"$tmp/out" 2>&1; then
    rel="$d/release"
    count="$(find "$rel" -type f | wc -l | tr -d ' ')"
    if [ "$count" -eq 8 ]; then pass "e release-assets made 8 files"; else
        fail "e expected 8 files, got $count"; ls "$rel"; fi
    if command -v sha256sum >/dev/null 2>&1; then sumcheck="sha256sum -c"; else
        sumcheck="shasum -a 256 -c"; fi
    if (cd "$rel" && $sumcheck SHA256SUMS >/dev/null 2>&1); then
        pass "e SHA256SUMS verifies with $sumcheck"
    else fail "e SHA256SUMS does not verify"; fi
    if grep -q "$name-$apkver-r0.x86_64.apk" "$rel/SHA256SUMS" &&
        grep -q "$name-$apkver-r0.aarch64.apk" "$rel/SHA256SUMS" &&
        ! grep -q 0.0.1 "$rel/SHA256SUMS"; then
        pass "e apk files carry the CPU type, the other version is not collected"
    else fail "e file names in SHA256SUMS"; fi
    mkdir "$tmp/vh"; chmod 700 "$tmp/vh"
    if GNUPGHOME="$tmp/vh" gpg --batch --quiet --import dist/keys/splicefs.asc 2>/dev/null &&
        GNUPGHOME="$tmp/vh" gpg --batch --verify "$rel/SHA256SUMS.asc" "$rel/SHA256SUMS" \
            2>/dev/null; then
        pass "e SHA256SUMS.asc verifies with the public key"
    else fail "e SHA256SUMS.asc does not verify"; fi
else
    fail "e release-assets.sh"; sed 's/^/     /' "$tmp/out"
fi

# (f) An incomplete repository is refused before anything is sent.
cp -R dist/repo "$tmp/partial"
rm "$tmp/partial/alpine/aarch64/APKINDEX.tar.gz"
if send "$tmp/partial" "$tmp/neverthere" >"$tmp/out" 2>&1 || [ -e "$tmp/neverthere" ]; then
    fail "f send must refuse an incomplete repository"
elif grep -q 'missing .*APKINDEX.tar.gz' "$tmp/out"; then
    pass "f send refuses a repository without alpine/aarch64/APKINDEX.tar.gz"
else
    fail "f send refused for the wrong reason"; sed 's/^/     /' "$tmp/out"
fi

[ "$failed" -eq 0 ] || { echo "$failed check(s) failed"; exit 1; }
echo "publish checks done"
