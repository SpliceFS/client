#!/bin/sh
# Reads the groups of the cluster scenarios: which scenarios run in which job of the
# integration test. The groups are the file scripts/cluster-groups, one line per scenario,
# "GROUP FUNCTION", where FUNCTION is the function the scenario is registered with in
# scripts/cluster-scenarios/. This is the one reader of that file.
#
#   cluster-groups.sh names              the groups, in the order of the file, one per line
#   cluster-groups.sh scenarios GROUP    the functions of the group, in the order of the file
#   cluster-groups.sh check              fails unless every registered scenario is in exactly
#                                        one group and every line names a registered scenario
#
# CLUSTER_GROUPS_FILE and CLUSTER_SCENARIOS_DIR replace the file and the folder, for the test.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
groups_file="${CLUSTER_GROUPS_FILE:-$here/cluster-groups}"
scenarios_dir="${CLUSTER_SCENARIOS_DIR:-$here/cluster-scenarios}"

usage() {
    echo "usage: cluster-groups.sh names | scenarios GROUP | check" >&2
    exit 2
}

# The lines of the file that name a scenario. A line that is not "GROUP FUNCTION" is a
# mistake, and is reported with its place.
entries() {
    awk '
        /^#/ || NF == 0 { next }
        NF != 2 { printf "%s:%d: want \"GROUP FUNCTION\"\n", FILENAME, FNR > "/dev/stderr"; bad = 1; next }
        { print }
        END { exit bad }
    ' "$groups_file"
}

# The functions that the scenario files register, one per line. A registration may go on
# in a second line ("scenario NAME \" and then the function), so the lines that end in a
# backslash are joined first. The function is the last word of the last line.
registered() {
    awk '
        { line = line $0 }
        line ~ /\\$/ { sub(/\\$/, "", line); next }
        line ~ /^scenario / { print $NF }
        { line = "" }
    ' "$scenarios_dir"/*.sh
}

names() {
    list="$(entries)"
    printf '%s\n' "$list" | awk 'NF && !seen[$1]++ { print $1 }'
}

scenarios() {
    list="$(entries)"
    found="$(printf '%s\n' "$list" | awk -v group="$1" '$1 == group { print $2 }')"
    if [ -z "$found" ]; then
        known="$(names)"
        known="$(printf '%s\n' "$known" | tr '\n' ' ')"
        echo "cluster-groups.sh: no group named \"$1\" in $groups_file; the groups are: $known" >&2
        exit 1
    fi
    printf '%s\n' "$found"
}

check() {
    list="$(entries)"
    [ -n "$list" ] || { echo "cluster-groups.sh: $groups_file holds no group" >&2; exit 1; }
    problems="$({
        printf '%s\n' "$list" | awk '{ print "listed", $2 }'
        registered | awk '{ print "registered", $1 }'
    } | awk '
        $1 == "listed" { listed[$2]++ }
        $1 == "registered" { registered[$2]++ }
        END {
            for (name in registered) {
                if (registered[name] > 1) print "registered more than once: " name
                if (!(name in listed)) print "in no group: " name
            }
            for (name in listed) {
                if (listed[name] > 1) print "listed more than once: " name
                if (!(name in registered)) print "listed, and registered nowhere: " name
            }
        }
    ' | sort)"
    if [ -n "$problems" ]; then
        echo "cluster-groups.sh: the groups in $groups_file do not match the scenarios:" >&2
        printf '%s\n' "$problems" >&2
        exit 1
    fi
    echo "cluster-groups: every scenario is in exactly one group"
}

[ "$#" -ge 1 ] || usage
case "$1" in
    names) [ "$#" -eq 1 ] || usage; names ;;
    scenarios) [ "$#" -eq 2 ] || usage; scenarios "$2" ;;
    check) [ "$#" -eq 1 ] || usage; check ;;
    *) usage ;;
esac
