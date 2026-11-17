#!/bin/sh
# Test of scripts/fuzz-targets.sh, fuzz-packages.sh and fuzz.sh with a stand-in for the go
# program that answers from a table of packages and fuzz targets and records every fuzz run.
# Nothing is compiled or fuzzed, so it runs in a moment, on the host, no Docker (make
# fuzz-test, part of make unit). What is checked is what the scripts decide: which targets and
# packages exist, which of them run, and with which arguments.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

mkdir "$work/bin"
cat > "$work/bin/go" <<'EOT'
#!/bin/sh
# list -m prints the module path. test -list prints, for the packages asked for, the targets of
# the table that match the pattern, then the line go prints after the tests of a package; a
# package that is not in the table is an error, as it is for go. A line of the table is
# "package target", or "package -" for a package without a fuzz target; the root package is
# ".". test -run records its arguments. LIST_FAILS and FUZZ_FAILS make the calls fail.
module=example.test/mod
case "$1 $2" in
"list -m")
    echo "$module"
    ;;
"test -list")
    [ -z "${LIST_FAILS:-}" ] || exit 1
    pattern="$3"
    shift 3
    awk -v pattern="$pattern" -v asked="$*" -v module="$module" '
        function flush() {
            if (current != "") {
                print "ok  \t" (current == "." ? module : module "/" current) "\t0.1s"
            }
        }
        BEGIN {
            n = split(asked, names, " ")
            for (i = 1; i <= n; i++) wanted[names[i]] = 1
        }
        asked != "./..." && !(("./" $1) in wanted) { next }
        { matched = 1 }
        $1 != current { flush(); current = $1 }
        $2 != "-" && $2 ~ pattern { print $2 }
        END {
            flush()
            if (!matched) { print "no such package: " asked > "/dev/stderr"; exit 1 }
        }
    ' "$TABLE"
    ;;
"test -run")
    echo "$*" >> "$CALLS"
    [ -z "${FUZZ_FAILS:-}" ] || exit 1
    ;;
esac
EOT
chmod +x "$work/bin/go"
export TABLE="$work/table"
export CALLS="$work/calls"
PATH="$work/bin:$PATH"

printf '%s\n' "alpha FuzzOne" "alpha FuzzTwo" "beta -" "gamma/deep FuzzDeep" ". FuzzRoot" > "$TABLE"

# $1 case name, $2 wanted status, $3 wanted output, then the command. A case that has to fuzz
# nothing checks CALLS itself.
check() {
    case_name="$1"
    want_status="$2"
    want_output="$3"
    shift 3
    : > "$CALLS"
    status=0
    got="$("$@" 2>&1)" || status=$?
    if [ "$status" -eq "$want_status" ] && [ "$got" = "$want_output" ]; then
        printf 'ok   %s\n' "$case_name"
    else
        printf 'FAIL %s: want status %s and "%s", got status %s and "%s"\n' \
            "$case_name" "$want_status" "$want_output" "$status" "$got"
        failed=$((failed + 1))
    fi
}

# $1 case name, $2 the calls that must have been recorded, as lines.
calls_are() {
    recorded="$(cat "$CALLS")"
    if [ "$recorded" = "$2" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s: want the calls "%s", got "%s"\n' "$1" "$2" "$recorded"
        failed=$((failed + 1))
    fi
}

check "lists every target as package:Target, the root package as a dot" 0 "alpha:FuzzOne
alpha:FuzzTwo
gamma/deep:FuzzDeep
.:FuzzRoot" "$root/scripts/fuzz-targets.sh"

check "lists nothing and fails when go cannot list" 1 "" env LIST_FAILS=1 "$root/scripts/fuzz-targets.sh"

check "lists only the targets of the named package" 0 "alpha:FuzzOne
alpha:FuzzTwo" "$root/scripts/fuzz-targets.sh" alpha

check "lists the targets of several named packages" 0 "alpha:FuzzOne
alpha:FuzzTwo
gamma/deep:FuzzDeep" "$root/scripts/fuzz-targets.sh" alpha gamma/deep

check "lists nothing for a package without fuzz targets" 0 "" "$root/scripts/fuzz-targets.sh" beta

check "fails for a package go does not know" 1 "no such package: ./nosuch" "$root/scripts/fuzz-targets.sh" nosuch

check "lists each package that has fuzz targets once, the root as a dot" 0 "alpha
gamma/deep
." "$root/scripts/fuzz-packages.sh"

check "lists no package and fails when go cannot list" 1 "" env LIST_FAILS=1 "$root/scripts/fuzz-packages.sh"

check "fuzzes every target when none is named" 0 "fuzz alpha FuzzOne (3s)
fuzz alpha FuzzTwo (3s)
fuzz gamma/deep FuzzDeep (3s)
fuzz . FuzzRoot (3s)" "$root/scripts/fuzz.sh" 3s
calls_are "gives each target its own package, pattern and time" "test -run ^\$ -fuzz ^FuzzOne\$ -fuzztime 3s ./alpha
test -run ^\$ -fuzz ^FuzzTwo\$ -fuzztime 3s ./alpha
test -run ^\$ -fuzz ^FuzzDeep\$ -fuzztime 3s ./gamma/deep
test -run ^\$ -fuzz ^FuzzRoot\$ -fuzztime 3s ./."

check "fuzzes only the named target" 0 "fuzz alpha FuzzTwo (1m)" "$root/scripts/fuzz.sh" 1m alpha:FuzzTwo
calls_are "runs nothing else" "test -run ^\$ -fuzz ^FuzzTwo\$ -fuzztime 1m ./alpha"

check "fuzzes several named targets in the order given" 0 "fuzz gamma/deep FuzzDeep (3s)
fuzz alpha FuzzOne (3s)" "$root/scripts/fuzz.sh" 3s gamma/deep:FuzzDeep alpha:FuzzOne

check "fuzzes every target of a named package, one after another" 0 "fuzz alpha FuzzOne (3s)
fuzz alpha FuzzTwo (3s)" "$root/scripts/fuzz.sh" 3s alpha
calls_are "and no target of another package" "test -run ^\$ -fuzz ^FuzzOne\$ -fuzztime 3s ./alpha
test -run ^\$ -fuzz ^FuzzTwo\$ -fuzztime 3s ./alpha"

check "fuzzes a package and a target of another together" 0 "fuzz gamma/deep FuzzDeep (3s)
fuzz alpha FuzzTwo (3s)" "$root/scripts/fuzz.sh" 3s gamma/deep alpha:FuzzTwo

check "refuses a package without fuzz targets, and fuzzes nothing" 1 \
    "beta has no fuzz targets (make -s fuzz-packages lists the packages)" "$root/scripts/fuzz.sh" 3s beta
calls_are "ran nothing for it" ""

check "stops when go does not know the package" 1 "no such package: ./nosuch" "$root/scripts/fuzz.sh" 3s nosuch

# Go passes a pattern that matches nothing with "no fuzz tests to fuzz", so a misspelled
# target would pass without fuzzing anything.
check "refuses a target the package does not have, and fuzzes nothing" 1 \
    "alpha:FuzzMissing is not a fuzz target (make -s fuzz-targets lists them)" \
    "$root/scripts/fuzz.sh" 3s alpha:FuzzMissing
calls_are "ran nothing for it" ""

check "refuses a target in a package without fuzz targets" 1 \
    "beta:FuzzOne is not a fuzz target (make -s fuzz-targets lists them)" "$root/scripts/fuzz.sh" 3s beta:FuzzOne

check "refuses a target without a package" 2 ":FuzzOne is not written as package:Target" \
    "$root/scripts/fuzz.sh" 3s :FuzzOne

check "refuses a target without a name" 2 "alpha: is not written as package:Target" \
    "$root/scripts/fuzz.sh" 3s alpha:

check "refuses every named target before fuzzing any" 1 \
    "alpha:FuzzMissing is not a fuzz target (make -s fuzz-targets lists them)" \
    "$root/scripts/fuzz.sh" 3s alpha:FuzzOne alpha:FuzzMissing
calls_are "so the valid one did not run either" ""

check "stops at the first failing target and passes the failure on" 1 "fuzz alpha FuzzOne (3s)" \
    env FUZZ_FAILS=1 "$root/scripts/fuzz.sh" 3s
calls_are "and ran no other" "test -run ^\$ -fuzz ^FuzzOne\$ -fuzztime 3s ./alpha"

check "refuses a call without a fuzz time" 2 "usage: fuzz.sh FUZZTIME [PACKAGE|PACKAGE:TARGET ...]" "$root/scripts/fuzz.sh"

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "fuzz scripts: all cases as expected"
