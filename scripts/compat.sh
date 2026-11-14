#!/bin/sh
# Decides which released servers the client of this working copy is tested against, builds
# both programs and runs scripts/compat-test.sh for each. Run it from anywhere; it works in
# the repository top. It needs Go, git, curl, and what compat-test.sh needs (Linux, root,
# FUSE).
#
# The rule it proves: with the same first version number, a client works with every server
# whose second number is equal or higher. So the server is released first, and a client
# whose second number is higher than any released server fails here. The runs are:
#   works    the oldest server this client may be used with (MAIN.MINOR.0), and the newest
#            server with the same first number
#   refused  the newest server with the same first number and a lower second number, which
#            this client must refuse
#
# SERVER_WORKS="tag tag" and SERVER_REFUSED="tag tag" replace that choice, to try the test
# before a release exists. A SERVER_REFUSED that is set and empty means no refusal run.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

# The version this client is about to get, as the release sets it in VERSION.
read -r version <VERSION
main="${version%%.*}"
rest="${version#*.}"
minor="${rest%%.*}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The tags of the server repository that are releases (v1.2.3), oldest first. sort -V is a
# GNU option, so the numbers are sorted field by field instead.
if [ -z "${SERVER_WORKS+set}" ] || [ -z "${SERVER_REFUSED+set}" ]; then
    remote="$(git ls-remote --tags --refs https://github.com/SpliceFS/server 'v*')" ||
        { echo "cannot list the tags of the server repository" >&2; exit 1; }
    tags="$(printf '%s\n' "$remote" |
        awk '{ sub("^refs/tags/", "", $2); if ($2 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) print substr($2, 2) }' |
        sort -t. -k1,1n -k2,2n -k3,3n |
        sed 's/^/v/')"
fi

if [ -z "${SERVER_WORKS+set}" ]; then
    oldest="v$main.$minor.0"
    found="$(printf '%s\n' "$tags" | awk -v tag="$oldest" '$0 == tag { print tag }')"
    if [ -z "$found" ]; then
        echo "the server is released first: no server $oldest exists, so a client $main.$minor must wait until the server is released at $main.$minor.0" >&2
        exit 1
    fi
    newest="$(printf '%s\n' "$tags" | awk -F. -v main="$main" 'substr($1, 2) == main { last = $0 } END { print last }')"
    SERVER_WORKS="$oldest"
    if [ "$newest" != "$oldest" ]; then SERVER_WORKS="$oldest $newest"; fi
fi

if [ -z "${SERVER_REFUSED+set}" ]; then
    SERVER_REFUSED="$(printf '%s\n' "$tags" |
        awk -F. -v main="$main" -v minor="$minor" \
            'substr($1, 2) == main && $2 + 0 < minor + 0 { last = $0 } END { print last }')"
fi

go build -buildvcs=false \
    -ldflags "-X github.com/SpliceFS/client/internal/version.version=v$version" \
    -o "$tmp/splicefs" ./cmd/splicefs

# Each server is built from its tag with that tag as its version, so that it reports what a
# release reports. A tag that is listed twice is built once.
build_server() {
    dir="$tmp/server-$1"
    if [ ! -x "$dir/splicefsd" ]; then
        git clone -q --depth 1 --branch "$1" https://github.com/SpliceFS/server "$dir/src"
        (cd "$dir/src" && go build -buildvcs=false \
            -ldflags "-X github.com/SpliceFS/server/internal/version.version=$1" \
            -o "$dir/splicefsd" ./cmd/splicefsd)
    fi
}

# The tags hold no spaces or glob characters, so splitting the lists is safe. ShellCheck
# does not know that, so the lists are read line by line instead.
run_all() {
    mode="$1"
    tag_list="$2"
    [ -n "$tag_list" ] || return 0
    printf '%s\n' "$tag_list" | tr ' ' '\n' >"$tmp/run-list"
    while read -r tag; do
        [ -n "$tag" ] || continue
        # Nothing inside may read the list that this loop is reading.
        build_server "$tag" </dev/null
        "$root/scripts/compat-test.sh" "$mode" "$tmp/server-$tag/splicefsd" "$tmp/splicefs" </dev/null
    done <"$tmp/run-list"
}

run_all works "$SERVER_WORKS"
run_all refused "$SERVER_REFUSED"

echo "compat: all runs as expected"
