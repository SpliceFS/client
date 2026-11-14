# shellcheck shell=sh
# Timers for the long test scripts, so that every run shows which part took how long. The file
# is sourced, so it runs nothing itself. Use:
#   timing_begin                    once, at the top of the script
#   timed LABEL COMMAND [ARG...]    runs the command and records how long it took
#   timing_mark LABEL SECONDS       records a span the script measured itself
#   timing_report TITLE             once, on the way out (in the EXIT trap)
#
# One record file is shared by every script of a run: the first script to call timing_begin
# makes it and owns it, and a script it starts finds TIMING_FILE in its environment and only
# appends. Each line is SECONDS, a tab, LABEL.
#
# A timed command runs exactly as it would without the timer: it is not made the left side
# of ||, so errexit stays on inside a timed function when the script set it. A test that
# relies on set -e to stop at its first failing command still stops there. The price: under
# set -e a failing command ends the script before its line is written, and the report shows
# the parts that finished.
#
# The total is the time from timing_begin to the report, not the sum of the lines, because
# a timed part may hold other timed parts.

# Not exported: a script started by this one must not think it owns the file.
timing_owner=""
timing_started=0

# The clock can be replaced by a file holding a number, so that the test of this file is exact
# and needs no sleep.
timing_now() {
    if [ -n "${TIMING_FAKE_NOW_FILE:-}" ]; then
        cat "$TIMING_FAKE_NOW_FILE"
    else
        date +%s
    fi
}

# 192 becomes 3m12s. Seconds have two digits so that the columns line up.
timing_format() {
    printf '%dm%02ds' "$(($1 / 60))" "$(($1 % 60))"
}

timing_begin() {
    timing_owner=""
    if [ -n "${TIMING_FILE:-}" ]; then
        return 0
    fi
    TIMING_FILE="$(mktemp)" || return 1
    export TIMING_FILE
    timing_started="$(timing_now)" || return 1
    timing_owner=1
}

# Called before timing_begin (or in a script that was never meant to be timed) it records
# nothing, so a timed call is safe anywhere.
timing_mark() {
    if [ -z "${TIMING_FILE:-}" ]; then
        return 0
    fi
    printf '%s\t%s\n' "$2" "$1" >>"$TIMING_FILE" || return 1
    timing_text="$(timing_format "$2")" || return 1
    printf '[time] %s: %s\n' "$1" "$timing_text" >&2
}

# $1 is the second the command started, $2 the label, the rest is the command. They stay in
# this function's own arguments, so that a timed command which calls timed itself cannot
# overwrite them.
timing_measure() {
    # Not "command || status=$?": that would switch errexit off inside the command. See the top.
    timing_command "$@"
    timing_status=$?
    timing_stop="$(timing_now)" || return 1
    timing_mark "$2" "$((timing_stop - $1))" || return 1
    return "$timing_status"
}

# Drops the start and the label and runs the command here, not in a subshell, so that a
# function can set variables for the script that called it.
timing_command() {
    shift 2
    "$@"
}

timed() {
    if [ -z "${TIMING_FILE:-}" ]; then
        shift
        "$@"
        return
    fi
    timing_first="$(timing_now)" || return 1
    timing_measure "$timing_first" "$@"
}

# Only the script that owns the record prints it, slowest part first, and then removes it.
# It is called from an EXIT trap, so nothing in it may end the script early: the exit status
# of the script must stay what it was.
timing_report() {
    if [ -z "$timing_owner" ] || [ ! -f "${TIMING_FILE:-}" ]; then
        return 0
    fi
    timing_tab="$(printf '\t')"
    # The label is the last key so that parts of equal length always come out in one order.
    timing_rows="$(sort -t "$timing_tab" -k1,1nr -k2 "$TIMING_FILE")" || timing_rows=""
    timing_end="$(timing_now)" || timing_end="$timing_started"
    timing_total="$(timing_format "$((timing_end - timing_started))")"
    printf '== Time: %s ==\n' "$1"
    timing_lines "  %s  %s\n" "$timing_rows" "$timing_total" "$timing_tab"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        {
            printf '### Time: %s\n\n| Time | Part |\n| --- | --- |\n' "$1"
            timing_lines "| %s | %s |\n" "$timing_rows" "$timing_total" "$timing_tab"
            printf '\n'
        } >>"$GITHUB_STEP_SUMMARY" || true
    fi
    rm -f "$TIMING_FILE" || true
    unset TIMING_FILE
    timing_owner=""
}

# $1 the printf format of one line (time, then label), $2 the sorted record, $3 the total
# as text, $4 the tab character. Prints every line and then the total.
timing_lines() {
    while IFS="$4" read -r timing_secs timing_name; do
        if [ -z "$timing_secs" ]; then
            continue
        fi
        timing_line_text="$(timing_format "$timing_secs")" || continue
        # The format is chosen by this file, never by the label.
        # shellcheck disable=SC2059
        printf "$1" "$timing_line_text" "$timing_name"
    done <<ROWS
$2
ROWS
    # shellcheck disable=SC2059
    printf "$1" "$3" "total"
}
