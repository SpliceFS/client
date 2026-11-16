#!/bin/sh
# Test of where the Makefile gets VERSION from. The version of a build is the file VERSION,
# with a v in front and -dirty behind when tracked files have uncommitted changes, whatever
# git knows about tags: CI checks out one commit and no tags. Runs on the host, no Docker
# (make version-test, part of make check). Every case builds a small repository of its own
# from the real Makefile and VERSION, so the state of this working tree does not matter.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# The machine's own git settings (signing, hooks) must not decide the result.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
failed=0

# Prints the version the Makefile in folder $1 makes. The Makefile has no target that prints
# it, so the target comes from standard input.
version_of() {
    # $(VERSION) is for make to expand, not the shell.
    # shellcheck disable=SC2016
    printf 'print-version:\n\t@echo $(VERSION)\n' |
        make -s -C "$1" -f Makefile -f - print-version
}

# $1 case name, $2 folder with the Makefile, $3 the version it must make.
expect() {
    got="$(version_of "$2")"
    if [ "$got" = "$3" ]; then
        printf 'ok   %-34s %s\n' "$1" "$got"
    else
        printf 'FAIL %-34s want "%s" got "%s"\n' "$1" "$3" "$got"
        failed=$((failed + 1))
    fi
}

git_in() {
    folder="$1"
    shift
    git -C "$folder" -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false "$@"
}

release="v$(cat "$root/VERSION")"

# A clean repository: one commit that holds a Makefile, VERSION and one more tracked file.
clean="$tmp/clean"
mkdir "$clean"
cp "$root/Makefile" "$root/VERSION" "$clean"
echo content >"$clean/tracked.txt"
git init -q "$clean"
git_in "$clean" add .
git_in "$clean" commit -q -m first

expect "clean tree" "$clean" "$release"

# CI: one commit, no history, no tags.
shallow="$tmp/shallow"
git clone -q --depth 1 --no-tags "file://$clean" "$shallow"
expect "one commit checked out, no tags" "$shallow" "$release"

# A tag, and a commit after it, must not change the version.
tagged="$tmp/tagged"
git clone -q "file://$clean" "$tagged"
git_in "$tagged" tag v9.9.9
git_in "$tagged" commit -q --allow-empty -m "after the tag"
expect "newer tag, commit after it" "$tagged" "$release"

# Not a git checkout at all, for example an unpacked source archive.
archive="$tmp/archive"
mkdir "$archive"
cp "$root/Makefile" "$root/VERSION" "$archive"
expect "no git checkout" "$archive" "$release"

# A new file git does not track does not make the build dirty, as before.
echo more >"$clean/untracked.txt"
expect "untracked file" "$clean" "$release"

echo changed >"$clean/tracked.txt"
expect "tracked file changed" "$clean" "$release-dirty"

# The caller can still name the version, as the package scripts do inside their containers.
got="$(VERSION=v7.7.7 version_of "$clean")"
if [ "$got" = v7.7.7 ]; then
    printf 'ok   %-34s %s\n' "version given by the caller" "$got"
else
    printf 'FAIL %-34s want "v7.7.7" got "%s"\n' "version given by the caller" "$got"
    failed=$((failed + 1))
fi

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "version: the Makefile takes it from the file VERSION"
