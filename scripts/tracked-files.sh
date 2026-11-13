#!/bin/sh
# Fails when the repository tracks a compiled program or a very large file. A program built
# on a developer's machine carries the folder names of that machine and has no place in the
# source; one was committed by mistake once, next to the code it was built from. It runs on
# the host (make tracked-files, part of make check) and needs git and od.
set -eu

# Larger than any source file here.
limit=1048576
# The loop below runs in a subshell, so what it finds is collected in a file.
found="$(mktemp)"
trap 'rm -f "$found"' EXIT

git ls-files | while read -r file; do
    [ -f "$file" ] || continue
    size="$(wc -c < "$file" | tr -d ' ')"
    if [ "$size" -gt "$limit" ]; then
        echo "tracked file larger than 1 MiB: $file ($size bytes)" >> "$found"
    fi
    # The first bytes say what a compiled program is: ELF (Linux, FreeBSD), Mach-O in its
    # three forms (macOS), and MZ (Windows).
    case "$(od -An -tx1 -N4 "$file" | tr -d ' \n')" in
        7f454c46 | cffaedfe | cefaedfe | cafebabe | 4d5a*)
            echo "tracked file is a compiled program: $file" >> "$found"
            ;;
        *) ;; # any other first bytes are not a compiled program
    esac
done

if [ -s "$found" ]; then
    cat "$found"
    exit 1
fi
echo "tracked files: no compiled program, nothing over 1 MiB"
