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

# dist/repo may come from make repo alone, without the FreeBSD folders that make
# package-freebsd adds. base is dist/repo plus a stand-in catalog and package for each
# FreeBSD folder that is not there, so every send that should pass has what it needs.
base="$tmp/base"
cp -R dist/repo "$base"
freebsd_dirs=""
majors=$(awk '!/^#/ && NF { split($1, v, "."); print v[1] }' \
    packaging/freebsd/images | sort -un)
for major in $majors; do
    for arch in amd64 aarch64; do
        d="freebsd/FreeBSD:$major:$arch"
        freebsd_dirs="$freebsd_dirs $d"
        if [ ! -f "$base/$d/meta.conf" ]; then
            mkdir -p "$base/$d/All"
            echo "meta" > "$base/$d/meta.conf"
            echo "pkg $major $arch" > "$base/$d/All/splicefs-server-0.0.1.pkg"
        fi
    done
done

chmod_ok=1
rsync --chmod=F644 --list-only "$tmp" >/dev/null 2>&1 || chmod_ok=0
# A refused send is a result to check, not a reason to stop. send always returns 0 and leaves
# the exit status of publish.sh in send_status, so no call of it sits in a condition, where
# set -e would be off inside it.
send_status=0
send() {
    send_status=0
    REPO_DIR="$1" UPLOAD_TARGET="$2" sh scripts/publish.sh send >"$tmp/out" 2>&1 || send_status=$?
}

# (a) A send to an empty folder reproduces dist/repo, with the modes the web server needs.
if [ "$chmod_ok" -eq 0 ]; then
    skip "a send to an empty folder: rsync has no --chmod"
    skip "b stale index removed, package missing locally refused: rsync has no --chmod"
    skip "c send refuses when a published package would disappear: rsync has no --chmod"
    cp -R "$base" "$tmp/target"
else
    mkdir "$tmp/target"
    send "$base" "$tmp/target"
    wrong_file_modes="$(find "$tmp/target" -type f ! -perm 644)"
    wrong_dir_modes="$(find "$tmp/target" -type d ! -perm 755)"
    if [ "$send_status" -eq 0 ] && diff -r "$base" "$tmp/target" &&
        [ -z "$wrong_file_modes" ] && [ -z "$wrong_dir_modes" ]; then
        pass "a send to an empty folder gives the same tree, modes 644 and 755"
    else
        fail "a send to an empty folder"; sed 's/^/     /' "$tmp/out"
    fi

    # (b) Index files that a new build replaces go; that is the only thing a send removes.
    echo stale > "$tmp/target/rpm/x86_64/repodata/stale-primary.xml.gz"
    send "$base" "$tmp/target"
    if [ "$send_status" -eq 0 ] &&
        [ ! -e "$tmp/target/rpm/x86_64/repodata/stale-primary.xml.gz" ] &&
        diff -r "$base" "$tmp/target"; then
        pass "b the stale repodata file is gone and nothing else changed"
    else
        fail "b stale repodata removed"; sed 's/^/     /' "$tmp/out"
    fi

    # A package that is published but missing here means the download step was skipped or
    # failed. Sending then would publish an index without it, so the send must refuse.
    old="apt/pool/main/old/old-package_0.0.1_amd64.deb"
    mkdir -p "$tmp/target/$(dirname "$old")"
    echo old > "$tmp/target/$old"
    send "$base" "$tmp/target"
    if [ "$send_status" -eq 0 ]; then
        fail "b send went ahead although a published package is missing locally"
    elif [ -f "$tmp/target/$old" ] && grep -q "$old" "$tmp/out"; then
        pass "b send refused because a published package is missing locally, and it is still there"
    else
        fail "b refusal for a package missing locally"; sed 's/^/     /' "$tmp/out"
    fi
    rm "$tmp/target/$old"
    rmdir "$tmp/target/$(dirname "$old")"

    # (c) A repository that lacks a published package is refused and nothing is deleted.
    cp -R "$base" "$tmp/copy"
    rpm="$(cd "$base" && find rpm/x86_64 -name "*.rpm" | head -n 1)"
    rm "$tmp/copy/$rpm"
    send "$tmp/copy" "$tmp/target"
    if [ "$send_status" -eq 0 ]; then
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
cp -R "$base" "$tmp/partial"
rm "$tmp/partial/alpine/aarch64/APKINDEX.tar.gz"
send "$tmp/partial" "$tmp/neverthere"
if [ "$send_status" -eq 0 ] || [ -e "$tmp/neverthere" ]; then
    fail "f send must refuse an incomplete repository"
elif grep -q 'missing .*APKINDEX.tar.gz' "$tmp/out"; then
    pass "f send refuses a repository without alpine/aarch64/APKINDEX.tar.gz"
else
    fail "f send refused for the wrong reason"; sed 's/^/     /' "$tmp/out"
fi

# (g) A FreeBSD catalog that is missing stops the send before anything is sent.
cp -R "$base" "$tmp/nofb"
fb_first="${freebsd_dirs# }"; fb_first="${fb_first%% *}"
rm "$tmp/nofb/$fb_first/meta.conf"
send "$tmp/nofb" "$tmp/neverthere2"
if [ "$send_status" -eq 0 ] || [ -e "$tmp/neverthere2" ]; then
    fail "g send must refuse a repository without a FreeBSD meta.conf"
elif grep -q "missing .*$fb_first/meta.conf" "$tmp/out" && grep -q 'make package-freebsd' "$tmp/out"; then
    pass "g send refuses a repository without $fb_first/meta.conf, and sends nothing"
else
    fail "g send refused for the wrong reason"; sed 's/^/     /' "$tmp/out"
fi

# (h) A published FreeBSD package that is missing locally is refused like any other package.
if [ "$chmod_ok" -eq 1 ]; then
    oldfb="$fb_first/All/old-package-0.0.1.pkg"
    echo old > "$tmp/target/$oldfb"
    send "$base" "$tmp/target"
    if [ "$send_status" -eq 0 ]; then
        fail "h send went ahead although a published FreeBSD package is missing locally"
    elif [ -f "$tmp/target/$oldfb" ] && grep -q "$oldfb" "$tmp/out"; then
        pass "h send refused because a published FreeBSD package is missing locally"
    else
        fail "h refusal for a FreeBSD package missing locally"; sed 's/^/     /' "$tmp/out"
    fi
    rm "$tmp/target/$oldfb"
else
    skip "h FreeBSD package missing locally refused: rsync has no --chmod"
fi

# (i) The upload goes in two passes and stops when one fails. A wrapper for rsync is put on the
# PATH: it counts the real passes (a dry run is not one), saves a copy of the target folder
# after each, and makes the pass named by FAIL_PASS fail, so the state between the passes can
# be looked at. The index files are named here again, as a person reading the layout would:
# the apt dists folder, the rpm repodata folders, APKINDEX.tar.gz, the FreeBSD catalog and the
# signature files next to them.
if [ "$chmod_ok" -eq 0 ]; then
    skip "i upload order and stop on error: rsync has no --chmod"
else
    real_rsync="$(command -v rsync)"
    mkdir "$tmp/wrap"
    cat > "$tmp/wrap/rsync" <<'WRAP'
#!/bin/sh
set -eu
# A dry run is not a pass, and neither is the second process that some rsync versions start
# for a local copy (it is called with --server).
for arg in "$@"; do
    case "$arg" in
        --dry-run | --server) exec "$REAL_RSYNC" "$@" ;;
        *) ;;
    esac
done
count=0
[ ! -f "$PASS_DIR/count" ] || count="$(cat "$PASS_DIR/count")"
count=$((count + 1))
echo "$count" > "$PASS_DIR/count"
[ "$count" != "${FAIL_PASS:-0}" ] || { echo "test wrapper: pass $count fails" >&2; exit 23; }
"$REAL_RSYNC" "$@"
for dest; do :; done
cp -R "$dest" "$PASS_DIR/after-$count"
WRAP
    chmod 755 "$tmp/wrap/rsync"
    # Sends $1 to $2 through the wrapper. $3 is the number of the pass that fails, if any. The
    # number of passes that were run is left in pass_total.
    send_logged() {
        rm -rf "$tmp/passes"
        mkdir "$tmp/passes"
        send_status=0
        PATH="$tmp/wrap:$PATH" REAL_RSYNC="$real_rsync" PASS_DIR="$tmp/passes" \
            FAIL_PASS="${3:-0}" REPO_DIR="$1" UPLOAD_TARGET="$2" \
            sh scripts/publish.sh send >"$tmp/out" 2>&1 || send_status=$?
        pass_total=0
        [ ! -f "$tmp/passes/count" ] || pass_total="$(cat "$tmp/passes/count")"
    }
    files_in() { (cd "$1" && find . -type f | sed 's|^\./||' | sort); }

    index_re='^(apt/dists/|rpm/[^/]+/repodata/'
    index_re="$index_re"'|alpine/[^/]+/(APKINDEX\.tar\.gz|[^/]+\.(sig|asc))$'
    index_re="$index_re"'|freebsd/[^/]+/(meta|meta\.conf|packagesite\.pkg|data\.pkg|[^/]+\.(sig|asc))$)'
    # The stand-in FreeBSD folders hold only meta.conf, so the other catalog files are added.
    order_repo="$tmp/order_repo"
    cp -R "$base" "$order_repo"
    for d in $freebsd_dirs; do
        for f in meta meta.conf packagesite.pkg data.pkg meta.conf.sig; do
            [ -f "$order_repo/$d/$f" ] || echo "index $f" > "$order_repo/$d/$f"
        done
    done
    files_in "$order_repo" > "$tmp/all_files"
    grep -E "$index_re" "$tmp/all_files" > "$tmp/index_files" || true
    grep -Ev "$index_re" "$tmp/all_files" > "$tmp/other_files" || true
    if grep -qx 'apt/dists/stable/InRelease' "$tmp/index_files" &&
        grep -q '^rpm/x86_64/repodata/repomd.xml.asc$' "$tmp/index_files" &&
        grep -q '^alpine/x86_64/APKINDEX.tar.gz$' "$tmp/index_files" &&
        grep -q '/packagesite.pkg$' "$tmp/index_files" &&
        ! grep -q '\.deb$\|\.rpm$\|\.apk$\|/All/' "$tmp/index_files" &&
        grep -q '\.deb$' "$tmp/other_files"; then
        pass "i the index files are told apart from the packages"
    else
        fail "i the list of index files is wrong"
    fi

    # The first publication: pass 1 leaves a folder without any index file, pass 2 completes it.
    mkdir "$tmp/first"
    send_logged "$order_repo" "$tmp/first"
    after_first="$(files_in "$tmp/passes/after-1")"
    want_first="$(cat "$tmp/other_files")"
    if [ "$send_status" -eq 0 ] && [ "$pass_total" = 2 ] && [ "$after_first" = "$want_first" ] &&
        diff -r "$order_repo" "$tmp/first" >/dev/null; then
        pass "i a send is two passes: all packages first, then the index files"
    else
        fail "i the passes of a first send (status $send_status, $pass_total passes)"
        sed 's/^/     /' "$tmp/out"
    fi

    # An upgrade: the index of the old release stays until pass 2, and so do the deletions.
    cp -R "$order_repo" "$tmp/upgrade"
    new_pkg="$(cd "$order_repo" && find rpm/x86_64 -name "*.rpm" | head -n 1)"
    rm "$tmp/upgrade/$new_pkg"
    old_text="the index of the release before"
    while read -r f; do echo "$old_text" > "$tmp/upgrade/$f"; done < "$tmp/index_files"
    stale=rpm/x86_64/repodata/stale-primary.xml.gz
    echo stale > "$tmp/upgrade/$stale"
    send_logged "$order_repo" "$tmp/upgrade"
    old_kept=1
    while read -r f; do
        grep -qx "$old_text" "$tmp/passes/after-1/$f" || old_kept=0
    done < "$tmp/index_files"
    if [ "$send_status" -eq 0 ] && [ "$pass_total" = 2 ] && [ "$old_kept" -eq 1 ] &&
        [ -f "$tmp/passes/after-1/$new_pkg" ] && [ -f "$tmp/passes/after-1/$stale" ] &&
        [ ! -e "$tmp/upgrade/$stale" ] && diff -r "$order_repo" "$tmp/upgrade" >/dev/null; then
        pass "i an upgrade: the new package is there before any new index, the deletion comes last"
    else
        fail "i the passes of an upgrade (status $send_status)"
        sed 's/^/     /' "$tmp/out"
    fi

    # A failed pass ends the script: no later pass runs and nothing is reported as published.
    mkdir "$tmp/fail1" "$tmp/fail2"
    send_logged "$order_repo" "$tmp/fail1" 1
    left="$(files_in "$tmp/fail1")"
    if [ "$send_status" -ne 0 ] && [ "$pass_total" = 1 ] && [ -z "$left" ] &&
        grep -q 'pass 1' "$tmp/out" && ! grep -q '^published to' "$tmp/out"; then
        pass "i when pass 1 fails the script stops and nothing was sent"
    else
        fail "i a failing pass 1 (status $send_status)"; sed 's/^/     /' "$tmp/out"
    fi
    send_logged "$order_repo" "$tmp/fail2" 2
    left="$(files_in "$tmp/fail2")"
    if [ "$send_status" -ne 0 ] && [ "$pass_total" = 2 ] && [ "$left" = "$want_first" ] &&
        grep -q 'pass 2' "$tmp/out" && ! grep -q '^published to' "$tmp/out"; then
        pass "i when pass 2 fails the script stops, and no index file was sent"
    else
        fail "i a failing pass 2 (status $send_status)"; sed 's/^/     /' "$tmp/out"
    fi
fi

[ "$failed" -eq 0 ] || { echo "$failed check(s) failed"; exit 1; }
echo "publish checks done"
