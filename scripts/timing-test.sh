#!/bin/sh
# Test of scripts/timing.sh. Runs on the host, no Docker, no sleeps (make timing-test, part of
# make check): the clock is a file holding a number, so every duration is exact.
# The snippets below are single-quoted on purpose: they expand in the shell that runs them.
# shellcheck disable=SC2016
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
clock="$work/clock"
errfile="$work/stderr"
tab="$(printf '\t')"
failed=0

# Runs the snippet in a fresh shell that has read timing.sh. TIMING_FILE is emptied first so
# that a test run from inside a timed script starts clean. clock N sets the fake time.
# Sets out (standard output), errout (standard error) and code (exit status).
run() {
    prelude='. "$TIMING_LIB"; clock() { echo "$1" >"$TIMING_FAKE_NOW_FILE"; }; '
    echo 100 >"$clock"
    code=0
    out="$(TIMING_LIB="$root/scripts/timing.sh" TIMING_FAKE_NOW_FILE="$clock" TIMING_FILE="" \
        GITHUB_STEP_SUMMARY="${summary:-}" sh -c "$prelude$1" 2>"$errfile")" || code=$?
    errout="$(cat "$errfile")"
}

# $1 name, $2 wanted text, $3 text found.
expect() {
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s\n     want: %s\n     got:  %s\n' "$1" "$2" "$3"
        failed=$((failed + 1))
    fi
}

summary=""

# A failing command: its status comes back unchanged and its time is still recorded.
run 'timing_begin; fails() { clock 105; return 3; }
status=0; timed "failing step" fails || status=$?
echo "status=$status"; cat "$TIMING_FILE"'
expect "status of a failing command" "status=3
5${tab}failing step" "$out"
expect "time line on standard error" "[time] failing step: 0m05s" "$errout"

# Under set -e the script ends at the failing command, with its status. No line is written
# for it: the command is run as it would be without the timer.
rec="$work/record"
: >"$rec"
run 'set -e; TIMING_FILE="'"$rec"'"; export TIMING_FILE
fails() { clock 112; return 4; }
timed "step under errexit" fails; echo "not reached"'
expect "errexit ends the script with the status" "4" "$code"
expect "errexit ends the script at the command" "" "$out"
recorded="$(cat "$rec")"
expect "errexit leaves no line for the failing command" "" "$recorded"

# errexit stays on inside a timed function: it stops at its first failing command, as it
# would untimed. A timer must never let a test go on past a failure.
run 'set -e; timing_begin; steps() { echo "one"; false; echo "two"; }
timed "function under errexit" steps; echo "not reached"'
expect "a timed function stops at its first failing command" "one" "$out"
expect "and the script ends with that status" "1" "$code"

run 'timing_begin; timed "good" true; echo "status=$?"'
expect "status of a command that works" "status=0" "$out"

# A function runs in the calling shell, so what it sets is still there afterwards.
run 'timing_begin; sets() { marker=set; clock 101; }; timed "function" sets
echo "marker=$marker"; cat "$TIMING_FILE"'
expect "a shell function is timed in the same shell" "marker=set
1${tab}function" "$out"

# The inner timed call must not overwrite the label or start of the outer one.
run 'timing_begin; inner_work() { clock 110; }
outer_work() { timed "inner" inner_work; clock 120; }
timed "outer" outer_work; cat "$TIMING_FILE"'
expect "timed inside timed keeps both lines" "10${tab}inner
20${tab}outer" "$out"

# A script started by a timed script appends to the same record, does not report and does
# not delete the file.
run 'timing_begin; timed "child script" sh -c '"'"'. "$TIMING_LIB"; timing_begin
timing_mark "child part" 7; timing_report "child"; echo "child saw $TIMING_FILE"'"'"'
f="$TIMING_FILE"; cat "$f"; timing_report "Parent"; [ -e "$f" ] || echo "file removed"'
case "$out" in
    *"== Time: child =="*) expect "nested script does not report" "no report" "reported" ;;
    *) expect "nested script does not report" "no report" "no report" ;;
esac
# The owner's record holds the child script's line and the parent's own line.
lines="$(printf '%s\n' "$out" | grep -c "^[0-9]*${tab}")"
expect "nested script appends to the owner's record" "2" "$lines"
last="$(printf '%s\n' "$out" | tail -n 1)"
expect "owner removes the record" "file removed" "$last"

# The report: slowest first, parts of equal length in label order, then the total, which is
# the time since timing_begin (the clock starts at 100) and not the sum of the lines.
run 'timing_begin; timing_mark "short" 45; timing_mark "long" 192; timing_mark "b tie" 5
timing_mark "a tie" 5; f="$TIMING_FILE"; clock 347; timing_report "Demo"
[ -e "$f" ] && echo "left behind" || echo "removed"'
expect "report order and total" "== Time: Demo ==
  3m12s  long
  0m45s  short
  0m05s  a tie
  0m05s  b tie
  4m07s  total
removed" "$out"

# The same table as Markdown, appended to what the summary file already holds.
summary="$work/summary"
echo "earlier text" >"$summary"
run 'timing_begin; timing_mark "short" 45; timing_mark "long" 192; clock 337; timing_report "Demo"'
written="$(cat "$summary")"
expect "Markdown summary" "earlier text
### Time: Demo

| Time | Part |
| --- | --- |
| 3m12s | long |
| 0m45s | short |
| 3m57s | total |" "$written"
summary=""

# Without timing_begin a timed command only runs.
run 'timed "x" sh -c '"'"'echo ran; exit 5'"'"' || echo "status=$?"; timing_mark "y" 1; timing_report "T"'
expect "timed before timing_begin runs the command" "ran
status=5" "$out"
expect "timed before timing_begin prints no time" "" "$errout"

# A script that does not own the record leaves it alone and prints nothing.
echo "9${tab}kept" >"$rec"
run 'TIMING_FILE="'"$rec"'"; export TIMING_FILE; timing_begin; timing_report "T"'
expect "a script that does not own the record prints nothing" "" "$out"
recorded="$(cat "$rec")"
expect "a script that does not own the record keeps it" "9${tab}kept" "$recorded"

# The report is meant for an EXIT trap, and the script keeps its own exit status.
run 'set -e; timing_begin; trap '"'"'timing_report "Trap"'"'"' EXIT
timed "doomed" sh -c '"'"'exit 4'"'"'; echo "not reached"'
expect "report from the exit trap" "== Time: Trap ==
  0m00s  total" "$out"
expect "exit status survives the trap" "4" "$code"

# The real clock prints whole seconds.
case "$(TIMING_FAKE_NOW_FILE="" sh -c ". '$root/scripts/timing.sh'; timing_now")" in
    "" | *[!0-9]*) expect "timing_now prints whole seconds" "digits" "other" ;;
    *) expect "timing_now prints whole seconds" "digits" "digits" ;;
esac

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "timing: all checks as expected"
