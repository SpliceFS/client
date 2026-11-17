#!/bin/sh
# Prints each package of the module that has a fuzz target, once, as fuzz-targets.sh names it.
# CI starts one job for each line; the packages come from the targets, so that there is one
# list and a package with no target gets no job.
set -eu

targets="$("$(dirname "$0")/fuzz-targets.sh")"
# The targets of a package are next to each other, so a repeat is the next line.
printf '%s\n' "$targets" | awk -F: 'NF && $1 != last { print $1 } { last = $1 }'
