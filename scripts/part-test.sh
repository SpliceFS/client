#!/bin/sh
# Test of scripts/part.sh, the helper every part target of the Makefile runs through. Runs on
# the host, no Docker (make part-test, part of make unit). The clock is a fake date program
# that answers from a prepared list, so the test does not depend on how long anything takes.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

mkdir "$work/bin"
cat > "$work/bin/date" <<'EOF'
#!/bin/sh
# Prints the first line of the file CLOCK and removes it from the file.
sed -n 1p "$CLOCK"
sed 1d "$CLOCK" > "$CLOCK.rest"
mv "$CLOCK.rest" "$CLOCK"
EOF
chmod +x "$work/bin/date"
export CLOCK="$work/clock"
PATH="$work/bin:$PATH"

# $1 case name, $2 first clock reading, $3 second, $4 expected status, $5 expected output, then
# the arguments of part.sh.
check() {
    case_name="$1"
    printf '%s\n%s\n' "$2" "$3" > "$CLOCK"
    want_status="$4"
    want_output="$5"
    shift 5
    status=0
    got="$("$root/scripts/part.sh" "$@" 2>&1)" || status=$?
    if [ "$status" -eq "$want_status" ] && [ "$got" = "$want_output" ]; then
        printf 'ok   %s\n' "$case_name"
    else
        printf 'FAIL %s: want status %s and "%s", got status %s and "%s"\n' \
            "$case_name" "$want_status" "$want_output" "$status" "$got"
        failed=$((failed + 1))
    fi
}

check "prints the seconds the command took" 100 107 0 "part demo: 7s" demo true
check "passes the output of the command on" 5 6 0 "hello
part demo: 1s" demo echo hello
check "keeps the arguments of the command whole" 5 5 0 "a b
part demo: 0s" demo printf '%s\n' "a b"
check "passes the failure status on and says so" 10 12 3 "part demo: 2s, failed with status 3" \
    demo sh -c 'exit 3'
check "refuses a call without a command" 1 1 2 "usage: part.sh NAME COMMAND [ARGUMENT ...]" demo
check "refuses a call without a name" 1 1 2 "usage: part.sh NAME COMMAND [ARGUMENT ...]"

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "part helper: all cases as expected"
