#!/bin/sh
# Test of the groups of the cluster scenarios: scripts/cluster-groups.sh, which reads the file
# scripts/cluster-groups, and how cluster-lib.sh and compat.sh use it. It runs on the host, no
# Docker, no mount and no network (make cluster-groups-test, part of make unit). The scenario
# files and the groups file are small files made here, so that every way the list can be wrong
# is tried, and then the real files must pass.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
failed=0

# Runs the command and passes when its status is $1 and its output (both streams) holds the
# text $2. An empty text is no condition on the output. The case name is $3.
expect() {
    want_status="$1"
    want_text="$2"
    case_name="$3"
    shift 3
    status=0
    "$@" >"$tmp/output" 2>&1 || status=$?
    if [ "$status" -eq "$want_status" ] && { [ -z "$want_text" ] || grep -qF -- "$want_text" "$tmp/output"; }; then
        printf 'ok   %s\n' "$case_name"
    else
        printf 'FAIL %s: want status %s and "%s", got status %s and:\n' "$case_name" "$want_status" "$want_text" "$status"
        cat "$tmp/output"
        failed=$((failed + 1))
    fi
}

# The output of the last expect, as one line, must be exactly the text.
output_is() {
    got="$(tr '\n' ' ' <"$tmp/output")"
    if [ "$got" = "$1" ]; then
        printf 'ok   %s\n' "$2"
    else
        printf 'FAIL %s: want "%s", got "%s"\n' "$2" "$1" "$got"
        failed=$((failed + 1))
    fi
}

# The scenario files: one registration on one line, one that goes on in a second line, and a
# comment that looks like a registration and is none.
scenarios="$tmp/scenarios"
mkdir "$scenarios"
cat >"$scenarios/10-a.sh" <<'EOF'
# scenario "not one" not_a_scenario
scenario "first one" first_fn
scenario "second one, registered over two lines" \
    second_fn
EOF
cat >"$scenarios/20-b.sh" <<'EOF'
scenario "third one" third_fn
EOF

groups="$tmp/groups"
groups_script="$root/scripts/cluster-groups.sh"
with_files() {
    CLUSTER_GROUPS_FILE="$groups" CLUSTER_SCENARIOS_DIR="$scenarios" "$groups_script" "$@"
}

cat >"$groups" <<'EOF'
# A comment, then a blank line.

g1 first_fn
g1 second_fn
g2 third_fn
EOF
expect 0 "" "the groups, in the order of the file" with_files names
output_is "g1 g2 " "the names are listed once each"
expect 0 "" "the scenarios of a group" with_files scenarios g1
output_is "first_fn second_fn " "the functions come in the order of the file"
expect 0 "every scenario is in exactly one group" "a list that is right passes the check" with_files check

expect 1 'no group named "g9"' "a group that does not exist is refused" with_files scenarios g9
expect 2 "usage" "a call without a command is refused" with_files
expect 2 "usage" "a command that does not exist is refused" with_files sort
expect 2 "usage" "a group is not given to names" with_files names g1
expect 2 "usage" "scenarios wants a group" with_files scenarios

printf 'g1 first_fn\ng1 second_fn\n' >"$groups"
expect 1 "in no group: third_fn" "a scenario in no group fails the check" with_files check

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\ng1 third_fn\n' >"$groups"
expect 1 "listed more than once: third_fn" "a scenario in two groups fails the check" with_files check

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\ng2 third_fn\n' >"$groups"
expect 1 "listed more than once: third_fn" "a scenario twice in one group fails the check" with_files check

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\ng2 no_such_fn\n' >"$groups"
expect 1 "listed, and registered nowhere: no_such_fn" "a name that no scenario has fails the check" with_files check

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\ng2 not_a_scenario\n' >"$groups"
expect 1 "listed, and registered nowhere: not_a_scenario" "a comment is not a registration" with_files check

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\n' >"$groups"
printf 'scenario "again" third_fn\n' >"$scenarios/30-c.sh"
expect 1 "registered more than once: third_fn" "a function registered twice fails the check" with_files check
rm "$scenarios/30-c.sh"

printf 'g1 first_fn second_fn\ng2 third_fn\n' >"$groups"
expect 1 'want "GROUP FUNCTION"' "a line with three words fails" with_files check
printf 'g1\n' >"$groups"
expect 1 'want "GROUP FUNCTION"' "a line with one word fails" with_files names
printf '# only a comment\n' >"$groups"
expect 1 "holds no group" "a file without a group fails the check" with_files check

# --- the real files -----------------------------------------------------------------------

expect 0 "every scenario is in exactly one group" "every real scenario is in exactly one real group" \
    "$groups_script" check

# compat.sh lists the parts of the integration test for CI: the compatibility runs, then the
# groups. It asks no server and builds nothing.
"$groups_script" names >"$tmp/real-groups"
{ echo compat; cat "$tmp/real-groups"; } >"$tmp/want-parts"
expect 0 "" "compat.sh lists its parts" "$root/scripts/compat.sh" parts
if cmp -s "$tmp/want-parts" "$tmp/output"; then
    echo "ok   the parts are compat and then every group"
else
    echo "FAIL the parts are not compat and then every group:"
    cat "$tmp/output"
    failed=$((failed + 1))
fi
expect 1 "is not a part of the integration test" "a part that does not exist is refused before anything is built" \
    env INTEGRATION_PART=no-such-part "$root/scripts/compat.sh"

# --- the filter of cluster-lib.sh ---------------------------------------------------------
# run_scenarios runs the scenarios of CLUSTER_GROUP, and those of CLUSTER_ONLY, and those that
# are in both. The scenarios here only write down that they ran, and the run ends the line
# that says which with a semicolon, so that a run of too many scenarios is not taken for a run
# of the right ones. Begin, end and the timer are stand-ins, because the real ones start
# servers and mount. The library reads the variable work, which is its own temporary folder.

run_scenarios_with() {
    (
        work="$1"
        CLUSTER_GROUP="$2"
        CLUSTER_ONLY="$3"
        export CLUSTER_GROUP CLUSTER_ONLY CLUSTER_GROUPS_FILE="$groups" CLUSTER_SCENARIOS_DIR="$scenarios"
        script_dir="$root/scripts"
        # shellcheck source=cluster-lib.sh
        . "$root/scripts/cluster-lib.sh"
        begin_scenario() { :; }
        end_scenario() { :; }
        timed() { shift; "$@"; }
        ran=""
        # The library calls the scenarios by the names they are registered with.
        # shellcheck disable=SC2329
        first_fn() { ran="$ran first"; }
        # shellcheck disable=SC2329
        second_fn() { ran="$ran second"; }
        # shellcheck disable=SC2329
        third_fn() { ran="$ran third"; }
        scenario "first one" first_fn
        scenario "second one" second_fn
        scenario "third one" third_fn
        run_scenarios
        echo "ran:$ran;"
    )
}

printf 'g1 first_fn\ng1 second_fn\ng2 third_fn\n' >"$groups"
for run in 1 2 3 4 5 6; do mkdir "$tmp/run$run"; done
expect 0 "ran: first second third;" "without a group every scenario runs" run_scenarios_with "$tmp/run1" "" ""
expect 0 "ran: first second;" "the scenarios of one group run" run_scenarios_with "$tmp/run2" g1 ""
expect 0 "ran: third;" "the other group runs the rest" run_scenarios_with "$tmp/run3" g2 ""
expect 0 "ran: second;" "a group and a text keep the scenarios that are in both" \
    run_scenarios_with "$tmp/run4" g1 second
expect 1 "no scenario matches" "a group and a text that share no scenario run nothing, and say so" \
    run_scenarios_with "$tmp/run5" g2 first
expect 1 'no group named "g9"' "an unknown group runs nothing" run_scenarios_with "$tmp/run6" g9 ""

if [ "$failed" -ne 0 ]; then
    echo "cluster groups: $failed case(s) failed" >&2
    exit 1
fi
echo "cluster groups: all cases as expected"
