#!/bin/sh
# Test of the time limits of the workflows (make workflow-timeout-test, part of make check, no
# Docker). A test that hangs keeps a job running for up to 6 hours, GitHub's default, so every
# job that runs on a runner has a job-level timeout-minutes, and a hang fails in minutes. A job
# that calls a reusable workflow (uses:) cannot have one; the jobs of the called workflow carry
# it. The check runs on the real workflows and on small workflows that break the rule.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
tree="$(mktemp -d)"
trap 'rm -rf "$tree"' EXIT
failed=0

# $1 a folder of workflow files. Prints one line for each job with runs-on and no timeout-minutes.
violations() {
    awk '
        function flush() {
            if (job != "" && runs && !limit) print where ": job " job " has no timeout-minutes"
            runs = 0; limit = 0
        }
        FNR == 1 { flush(); job = "" }
        /^[^ #]/ { flush(); job = "" }
        /^  [A-Za-z0-9_-]+:[ \t]*$/ { flush(); job = $1; sub(/:$/, "", job); where = FILENAME }
        /^    runs-on:/ { runs = 1 }
        /^    timeout-minutes:/ { limit = 1 }
        END { flush() }
    ' "$1"/*.yml
}

# $1 case name, $2 how many violation lines the folder $3 must show.
check() {
    got="$(violations "$3" | wc -l | tr -d ' ')"
    if [ "$got" -ne "$2" ]; then
        printf 'FAIL %s: %s violation(s), wanted %s\n' "$1" "$got" "$2"
        violations "$3" | sed 's/^/     /'
        failed=$((failed + 1))
    else
        printf 'ok   %s\n' "$1"
    fi
}

# $1 the folder, then the lines of the jobs, as printf arguments of one format.
workflow() {
    mkdir -p "$1"
    printf 'jobs:\n%b' "$2" > "$1/a.yml"
}

check "the workflows of this repository" 0 "$root/.github/workflows"

workflow "$tree/good" '  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10\n    steps:\n      - run: true\n'
check "a job with a limit" 0 "$tree/good"

workflow "$tree/order" '  a:\n    timeout-minutes: 10\n    runs-on: ubuntu-latest\n'
check "the limit before runs-on" 0 "$tree/order"

workflow "$tree/missing" '  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n'
check "a job without a limit" 1 "$tree/missing"

workflow "$tree/step" '  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n        timeout-minutes: 5\n'
check "a limit on a step is not a limit on the job" 1 "$tree/step"

workflow "$tree/caller" '  a:\n    uses: ./.github/workflows/b.yml\n'
check "a job that calls a workflow" 0 "$tree/caller"

workflow "$tree/two" '  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10\n  b:\n    runs-on: ubuntu-latest\n'
check "one job of two without a limit" 1 "$tree/two"

[ "$failed" -eq 0 ] || { echo "$failed check(s) failed"; exit 1; }
echo "workflow timeout checks done"
