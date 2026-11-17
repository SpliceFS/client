#!/bin/sh
# Decides which released servers the client of this working copy is tested against, builds
# both programs and runs scripts/compat-test.sh for each. Run it from anywhere; it works in
# the repository top. It needs Go, git, curl, and what compat-test.sh needs (Linux, root,
# FUSE).
#
# The rule it proves: with the same first version number, a client works with every server
# whose second number is equal or higher. So the server is released first, and a client
# whose second number is higher than any released server fails here. The runs are:
#   works    the oldest released server with the same first and second number, and the
#            newest released server with the same first number
#   refused  the newest server with the same first number and a lower second number, which
#            this client must refuse
#
# Then several servers as one cluster are tried (scripts/cluster-test.sh), with the newest of
# the servers that work; its scenarios are in groups (scripts/cluster-groups).
#
# SERVER_WORKS="tag tag" and SERVER_REFUSED="tag tag" replace that choice, to try the test
# before a release exists. A SERVER_REFUSED that is set and empty means no refusal run.
#
# INTEGRATION_PART=compat runs only the runs above, and INTEGRATION_PART=GROUP only the cluster
# scenarios of that group, which is how CI runs the test as several jobs. Empty runs it all.
# "compat.sh parts" prints the parts, one per line, and runs nothing else.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

# The compatibility runs, then each group of the cluster scenarios.
parts() {
    echo compat
    "$root/scripts/cluster-groups.sh" names
}
if [ "${1:-}" = parts ]; then
    parts
    exit 0
fi

part="${INTEGRATION_PART:-}"
part_list="$(parts)"
if [ -n "$part" ] && ! printf '%s\n' "$part_list" | grep -qxF "$part"; then
    known="$(printf '%s\n' "$part_list" | tr '\n' ' ')"
    echo "compat.sh: INTEGRATION_PART=$part is not a part of the integration test; the parts are: $known" >&2
    exit 1
fi

# shellcheck source=timing.sh
. "$root/scripts/timing.sh"

# The version this client is about to get, as the release sets it in VERSION.
read -r version <VERSION
main="${version%%.*}"
rest="${version#*.}"
minor="${rest%%.*}"

tmp="$(mktemp -d)"

# Runs on every way out, so that a failed run reports the times of the parts that did run.
finish() {
    status=$?
    trap - EXIT INT TERM
    rm -rf "$tmp"
    timing_report "The real client against the real server"
    exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The scripts started below add their own lines to the same record.
timing_begin

# The tags of the server repository that are versions (v1.2.3), oldest first. sort -V is a
# GNU option, so the numbers are sorted field by field instead.
if [ -z "${SERVER_WORKS+set}" ] || [ -z "${SERVER_REFUSED+set}" ]; then
    # Asked from the temporary folder: in a copy of a working tree whose git folder is
    # elsewhere, git would fail here before it ever looked at the address.
    remote="$(git -C "$tmp" ls-remote --tags --refs https://github.com/SpliceFS/server 'v*')" ||
        { echo "cannot list the tags of the server repository" >&2; exit 1; }
    tags="$(printf '%s\n' "$remote" |
        awk '{ sub("^refs/tags/", "", $2); if ($2 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/) print substr($2, 2) }' |
        sort -t. -k1,1n -k2,2n -k3,3n |
        sed 's/^/v/')"
fi

# Whether the server of a tag is released. A tag is not enough: the bot makes it at the merge,
# minutes before the server's release workflow publishes anything, and it stays when that
# release fails. The GitHub release with its checksum file is the last step of a server
# release, after its packages are published. Prints yes or no; any other answer ends the run.
released() {
    code="$(curl -sSL --proto '=https' --proto-redir '=https' --retry 3 -o /dev/null -w '%{http_code}' \
        "https://github.com/SpliceFS/server/releases/download/$1/SHA256SUMS" </dev/null)" ||
        { echo "cannot ask GitHub whether server $1 is released" >&2; exit 1; }
    case "$code" in
    200) echo yes ;;
    404) echo no ;;
    *) echo "GitHub answered $code when asked whether server $1 is released" >&2; exit 1 ;;
    esac
}

# The first released tag of the tags on standard input, in their order, or nothing. Only the
# tags it has to look at are asked about.
first_released() {
    cat >"$tmp/candidates"
    while read -r tag; do
        [ -n "$tag" ] || continue
        state="$(released "$tag")"
        if [ "$state" = yes ]; then
            printf '%s\n' "$tag"
            return 0
        fi
    done <"$tmp/candidates"
}
# The newest released tag of the tags on standard input (oldest first), or nothing.
newest_released() {
    awk '{ line[NR] = $0 } END { for (i = NR; i > 0; i--) print line[i] }' | first_released
}

if [ -z "${SERVER_WORKS+set}" ]; then
    # Not MAIN.MINOR.0: older releases of a series may have been removed, and the rule is
    # about the servers that can be installed.
    oldest="$(printf '%s\n' "$tags" |
        awk -F. -v main="$main" -v minor="$minor" 'substr($1, 2) == main && $2 + 0 == minor + 0' |
        first_released)"
    if [ -z "$oldest" ]; then
        echo "the server is released first: no server $main.$minor is released, so a client $main.$minor must wait until one is published" >&2
        exit 1
    fi
    newest="$(printf '%s\n' "$tags" | awk -F. -v main="$main" 'substr($1, 2) == main' | newest_released)"
    SERVER_WORKS="$oldest"
    if [ "$newest" != "$oldest" ]; then SERVER_WORKS="$oldest $newest"; fi
fi

if [ -z "${SERVER_REFUSED+set}" ]; then
    SERVER_REFUSED="$(printf '%s\n' "$tags" |
        awk -F. -v main="$main" -v minor="$minor" 'substr($1, 2) == main && $2 + 0 < minor + 0' |
        newest_released)"
fi

timed "build the client" go build -buildvcs=false \
    -ldflags "-X github.com/SpliceFS/client/internal/version.version=v$version" \
    -o "$tmp/splicefs" ./cmd/splicefs

# Each server is built from its tag with that tag as its version, so that it reports what a
# release reports. A tag that is listed twice is built once. Every program of the tag goes
# into one folder, so that a newer server's splicefsdctl sits next to its splicefsd, where
# server_hash_password looks for it.
build_server() {
    dir="$tmp/server-$1"
    if [ ! -x "$dir/splicefsd" ]; then
        git clone -q --depth 1 --branch "$1" https://github.com/SpliceFS/server "$dir/src"
        (cd "$dir/src" && go build -buildvcs=false \
            -ldflags "-X github.com/SpliceFS/server/internal/version.version=$1" \
            -o "$dir/" ./cmd/...)
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
        timed "build the released server $tag" build_server "$tag" </dev/null
        timed "compatibility run $mode, server $tag" \
            "$root/scripts/compat-test.sh" "$mode" "$tmp/server-$tag/splicefsd" "$tmp/splicefs" </dev/null
    done <"$tmp/run-list"
}

if [ -z "$part" ] || [ "$part" = compat ]; then
    run_all works "$SERVER_WORKS"
    run_all refused "$SERVER_REFUSED"
fi

# Several servers as one cluster are tried with the newest server this client works with:
# all running, some of the listed ones not running, and servers that hold different files.
# The server was built above, or is built here when only the cluster is tried.
if [ "$part" != compat ]; then
    cluster_tag="$(printf '%s\n' "$SERVER_WORKS" | tr ' ' '\n' | awk 'NF { last = $0 } END { print last }')"
    if [ -n "$cluster_tag" ]; then
        [ -x "$tmp/server-$cluster_tag/splicefsd" ] ||
            timed "build the released server $cluster_tag" build_server "$cluster_tag" </dev/null
        CLUSTER_GROUP="$part" "$root/scripts/cluster-test.sh" "$tmp/server-$cluster_tag/splicefsd" "$tmp/splicefs" </dev/null
    fi
fi

echo "compat: all runs as expected"
