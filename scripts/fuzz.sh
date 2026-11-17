#!/bin/sh
# Usage: fuzz.sh FUZZTIME [PACKAGE|PACKAGE:TARGET ...]
# Fuzzes each named target for FUZZTIME, and every target of each named package, one after
# another. With nothing named it fuzzes every target of the module. Go fuzzes one target per
# call, so this loops. A target that finds a failing input fails the script at once, and Go has
# written the input to testdata/fuzz in the package's folder.
set -eu

if [ "$#" -lt 1 ]; then
    echo "usage: fuzz.sh FUZZTIME [PACKAGE|PACKAGE:TARGET ...]" >&2
    exit 2
fi
fuzztime="$1"
shift
targets_of="$(dirname "$0")/fuzz-targets.sh"

# Prints the targets that $1 names: the target itself, or those of the package. Go passes a
# pattern that matches nothing with "no fuzz tests to fuzz", so a misspelled target would pass
# without fuzzing anything; the list of the package, which is what fuzz-targets.sh reads, says
# what exists.
resolve() {
    case "$1" in
    *:*)
        case "$1" in
        ?*:?*) ;;
        *)
            echo "$1 is not written as package:Target" >&2
            exit 2
            ;;
        esac
        found="$("$targets_of" "${1%%:*}")" || exit 1
        if ! printf '%s\n' "$found" | grep -Fqx "$1"; then
            echo "$1 is not a fuzz target (make -s fuzz-targets lists them)" >&2
            exit 1
        fi
        printf '%s\n' "$1"
        ;;
    *)
        found="$("$targets_of" "$1")" || exit 1
        if [ -z "$found" ]; then
            echo "$1 has no fuzz targets (make -s fuzz-packages lists the packages)" >&2
            exit 1
        fi
        printf '%s\n' "$found"
        ;;
    esac
}

if [ "$#" -gt 0 ]; then
    targets=""
    # Every name is checked before any target is fuzzed.
    for named in "$@"; do
        resolved="$(resolve "$named")"
        targets="$targets$resolved
"
    done
else
    targets="$("$targets_of")"
fi

for target in $targets; do
    package="${target%%:*}"
    name="${target#*:}"
    echo "fuzz $package $name ($fuzztime)"
    go test -run '^$' -fuzz "^$name\$" -fuzztime "$fuzztime" "./$package"
done
