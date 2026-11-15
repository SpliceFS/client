#!/bin/sh
# Moves the files of the FreeBSD package repository between the output folder and the build
# machine. It runs as root inside the Linux container, with the output folder at /out,
# started by package-freebsd.sh. It cannot run on the host: on Linux the signing key and
# everything a container wrote belong to root, so the host user can neither read the key nor
# remove what a machine returned.
#
#   freebsd-repo-files.sh input <major> <archive>   writes /out/<archive>, the machine's input:
#       the signing keys and the packages published before for this FreeBSD major version
#   freebsd-repo-files.sh merge                      merges what the machine returned in
#       /out/freebsd/repo into /out/repo/freebsd and removes the returned copy
set -eu

out=/out
published="$out/repo/freebsd"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# Writes the input archive of one machine.
input_archive() {
    major="$1"
    archive="$out/$2"
    stage="$out/freebsd-input/$major"
    # Folders left by an earlier run must not be merged again as if this run had made them.
    rm -rf "$stage" "$out"/freebsd/repo/FreeBSD:"$major":*
    mkdir -p "$stage/keys" "$stage/repo"
    cp "$out/keys/splicefs.rsa" "$out/keys/splicefs.rsa.pub" "$stage/keys/"
    for abiarch in amd64 aarch64; do
        if [ -d "$published/FreeBSD:$major:$abiarch" ]; then
            cp -R "$published/FreeBSD:$major:$abiarch" "$stage/repo/"
        fi
    done
    # The archive holds the private key, so only root may read it. A folder shared from macOS
    # can show attributes of that system, which FreeBSD's tar reports as errors when it
    # unpacks; the option leaves them out.
    rm -f "$archive"
    ( umask 077 && tar --no-xattrs -cf "$archive" -C "$stage" . )
    rm -rf "$stage"
}

# Merges the repository folders the machine returned into the published ones. The catalog
# files are replaced, packages are added. A published package is never replaced by a different
# file: a package under a name that exists must be byte for byte the same one.
merge_repo() {
    for src in "$out"/freebsd/repo/FreeBSD:*; do
        [ -d "$src" ] || continue
        dest="$published/$(basename "$src")"
        mkdir -p "$dest/All"
        for pkgfile in "$src"/All/*.pkg; do
            [ -f "$pkgfile" ] || continue
            target="$dest/All/$(basename "$pkgfile")"
            if [ -f "$target" ]; then
                cmp -s "$pkgfile" "$target" ||
                    fail "$target is published and differs from the package just built; a published package is never replaced"
            else
                cp "$pkgfile" "$target"
            fi
        done
        # Catalog files keep no old content: only what this run wrote stays.
        for old in "$dest"/*; do
            [ "$(basename "$old")" = All ] || rm -rf "$old"
        done
        for new in "$src"/*; do
            [ "$(basename "$new")" = All ] || cp -R "$new" "$dest/"
        done
    done
    # Merged, so the copy goes: its folder names hold colons, which the file upload of a CI
    # job refuses, and /out/freebsd is what such a job keeps.
    rm -rf "$out/freebsd/repo"
    # The host reads the repository as its own user.
    chmod -R a+rX "$published"
}

case "${1:-}" in
    input) input_archive "${2:?major}" "${3:?archive}" ;;
    merge) merge_repo ;;
    *) fail "usage: freebsd-repo-files.sh input <major> <archive> | merge" ;;
esac
