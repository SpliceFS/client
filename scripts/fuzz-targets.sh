#!/bin/sh
# Usage: fuzz-targets.sh [PACKAGE ...]
# Prints every fuzz target of the module, or only those of the named packages, one line each,
# as PACKAGE:Target. PACKAGE is the folder relative to the module root ("." for the root
# itself). The targets are found in the code, so a new Fuzz function is a new line and nothing
# else has to know about it. This is the one place that lists them: fuzz-packages.sh and
# fuzz.sh read it, and so does CI through them. It needs the go program, so a machine without
# Go runs it in the image (make -s fuzz-targets).
set -eu

module="$(go list -m)"
if [ "$#" -eq 0 ]; then
    set -- ./...
else
    # go wants a path that starts with ./ for a folder; each argument moves to the end of the
    # list in that form.
    for package in "$@"; do
        set -- "$@" "./$package"
        shift
    done
fi
# The list is read whole first: a package that does not compile fails the script here, and no
# half of a list is printed.
listing="$(go test -list '^Fuzz' "$@")"

# go prints the targets of a package, then a line "ok PACKAGE TIME" for it.
printf '%s\n' "$listing" | awk -v module="$module" '
    /^Fuzz/ { held = held $0 "\n"; next }
    $1 == "ok" {
        package = ($2 == module) ? "." : substr($2, length(module) + 2)
        n = split(held, names, "\n")
        for (i = 1; i < n; i++) print package ":" names[i]
    }
    $1 == "ok" || $1 == "?" || $1 == "FAIL" { held = "" }
'
