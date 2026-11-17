#!/bin/sh
# Runs a command and prints how long it took, as "part NAME: Ns". Every part target of the
# Makefile runs through this script, so the line is the same for every part, on a developer's
# machine and in CI. The status of the command is passed on, and a failure is said in the line.
set -eu

if [ "$#" -lt 2 ]; then
    echo "usage: part.sh NAME COMMAND [ARGUMENT ...]" >&2
    exit 2
fi
name="$1"
shift

start="$(date +%s)"
status=0
"$@" || status=$?
seconds=$(($(date +%s) - start))

if [ "$status" -eq 0 ]; then
    echo "part $name: ${seconds}s"
else
    echo "part $name: ${seconds}s, failed with status $status"
fi
exit "$status"
