#!/bin/sh
# Fails when the repository tracks something that must never be published: a compiled
# program, a very large file, or a file that names a private machine, account or place. A
# program built on a developer's machine carries the folder names of that machine; one was
# committed by mistake once, next to the code it was built from, and the ignore rules alone
# did not stop it. It runs on the host (make tracked-files, part of make check) and needs
# git, od and grep.
set -eu

# Larger than any source file here.
limit=1048576

# Names that belong to the private systems of the people who work on this project. Each is
# put together from two pieces, so that this file does not hold the names it looks for.
# The copyright line in LICENSE names the company on purpose and is the one exception.
private_names=""
for pieces in blue:rival blue:xd hil:doer pho:enix new-:york castle-:rock; do
    private_names="$private_names ${pieces%%:*}${pieces#*:}"
done

# The loop below runs in a subshell, so what it finds is collected in a file.
found="$(mktemp)"
trap 'rm -f "$found"' EXIT

git ls-files | while read -r file; do
    [ -f "$file" ] || continue
    size="$(wc -c < "$file")"
    size="$(printf '%s' "$size" | tr -d ' ')"
    if [ "$size" -gt "$limit" ]; then
        echo "tracked file larger than 1 MiB: $file ($size bytes)" >> "$found"
    fi
    # The first bytes say what a compiled program is: ELF (Linux, FreeBSD), Mach-O in its
    # three forms (macOS), and MZ (Windows).
    magic="$(od -An -tx1 -N4 "$file")"
    magic="$(printf '%s' "$magic" | tr -d ' \n')"
    case "$magic" in
        7f454c46 | cffaedfe | cefaedfe | cafebabe | 4d5a*)
            echo "tracked file is a compiled program: $file" >> "$found"
            ;;
        *) ;;
    esac
    if [ "$file" != LICENSE ]; then
        for name in $private_names; do
            # -a reads a binary file as text, so a name inside a program is found too.
            if grep -a -i -q -e "$name" "$file"; then
                echo "tracked file holds a private name: $file" >> "$found"
                break
            fi
        done
    fi
done

if [ -s "$found" ]; then
    cat "$found"
    exit 1
fi
echo "tracked files: no compiled program, nothing over 1 MiB, no private name"
