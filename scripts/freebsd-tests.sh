#!/bin/sh
# Builds the test program of every package for FreeBSD on amd64, the CPU type of the FreeBSD
# machines, and packs them, with the source they read their test files from, into one archive:
# OUTPUT_DIR/in.tar. The FreeBSD machine of scripts/check-freebsd.sh unpacks the archive and
# runs the programs. It runs in the Go image (make check-freebsd), in the source folder.
set -eu

out="${1:?usage: freebsd-tests.sh OUTPUT_DIR}"

# Prints how long the phase that just ended took. timing.sh is not used: the host script
# that reads the timing record is another process, so the lines are left in the output.
phase_start="$(date +%s)"
phase_done() {
    phase_now="$(date +%s)"
    echo "[time] $1: $((phase_now - phase_start))s"
    phase_start="$phase_now"
}

stage="$out/stage"
rm -rf "$stage" "${out:?}/in.tar"
mkdir -p "$stage/tests"
: > "$stage/tests/list"
go list -f '{{.Dir}}' ./... | while read -r dir; do
    rel="${dir#"$PWD"}"
    rel="${rel#/}"
    name="$(printf '%s' "${rel:-root}" | tr / _)"
    # No race detector: it needs C code, which a build for another system cannot have.
    # A package without tests writes no program and is left out.
    CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go test -c -covermode=atomic \
        -o "$stage/tests/$name.test" "./$rel" >/dev/null
    if [ -f "$stage/tests/$name.test" ]; then
        echo "$name ${rel:-.}" >> "$stage/tests/list"
    fi
done
phase_done "build the FreeBSD test programs"
tar -cf "$out/in.tar" --exclude=./.git --exclude=./.cache --exclude=./dist --exclude=./bin .
tar -rf "$out/in.tar" -C "$stage" tests
rm -rf "$stage"
members="$(tar -tf "$out/in.tar")"
programs="$(printf '%s\n' "$members" | grep -c '\.test$' || true)"
echo "FreeBSD test programs: $programs"
phase_done "pack the archive"
