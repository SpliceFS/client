#!/bin/sh
# Test of scripts/build-all.sh with a stand-in for the go program that records how it was
# called and writes an empty output file. Nothing is compiled, so it runs in a moment, on the
# host, no Docker (make build-all-test, part of make unit). What is checked is what the script
# decides: which programs, for which systems and CPU types, into which files, and with which
# flags.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# A small repository of its own: two programs, one of them with a service unit, a package name
# and the real libraries.
tree="$work/tree"
mkdir -p "$tree/scripts" "$tree/cmd/alpha" "$tree/cmd/beta" "$tree/packaging/deb" \
    "$tree/packaging/systemd" "$work/bin"
cp "$root/scripts/build-all.sh" "$root/scripts/package-lib.sh" "$tree/scripts/"
printf 'Package: splicefs-demo\n' > "$tree/packaging/deb/control"
printf '[Service]\n' > "$tree/packaging/systemd/alpha.service"
printf 'conf.d\n' > "$tree/packaging/config-folders"

cat > "$work/bin/go" <<'EOT'
#!/bin/sh
# list -m prints the module path; build records the target system and its arguments, then
# writes the file named by -o.
if [ "$1" = list ]; then
    echo example.test/demo
    exit 0
fi
out=""
prev=""
for arg in "$@"; do
    if [ "$prev" = -o ]; then out="$arg"; fi
    prev="$arg"
done
echo "$GOOS/$GOARCH cgo=$CGO_ENABLED $*" >> "$CALLS"
: > "$out"
EOT
chmod +x "$work/bin/go"
export CALLS="$work/calls"
: > "$CALLS"

(
    cd "$tree"
    PATH="$work/bin:$PATH" VERSION=v1.2.3 PACKAGE_DATE=20261112 LDFLAGS="-s -w -X example.test/demo/v.v=v1.2.3" \
        sh scripts/build-all.sh "$work/out"
)

# $1 case name, $2 what must be true of the calls: a fixed text that must appear in them.
has_call() {
    if grep -F -q -- "$2" "$CALLS"; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s: no call holds "%s"\n' "$1" "$2"
        failed=$((failed + 1))
    fi
}

# $1 case name, $2 what must not be in any call.
lacks_call() {
    if grep -F -q -- "$2" "$CALLS"; then
        printf 'FAIL %s: a call holds "%s"\n' "$1" "$2"
        failed=$((failed + 1))
    else
        printf 'ok   %s\n' "$1"
    fi
}

for os in linux freebsd darwin; do
    for arch in amd64 arm64; do
        for program in alpha beta; do
            if [ -f "$work/out/$os-$arch/$program" ]; then
                printf 'ok   writes %s-%s/%s\n' "$os" "$arch" "$program"
            else
                printf 'FAIL writes %s-%s/%s: the file is missing\n' "$os" "$arch" "$program"
                failed=$((failed + 1))
            fi
        done
    done
done

count="$(wc -l < "$CALLS" | tr -d ' ')"
if [ "$count" -eq 12 ]; then
    echo "ok   builds 12 times, once for each program, system and CPU type"
else
    echo "FAIL builds $count times, not 12"
    failed=$((failed + 1))
fi

has_call "static binary, paths trimmed" "linux/arm64 cgo=0 build -trimpath -ldflags -s -w -X example.test/demo/v.v=v1.2.3 -o $work/out/linux-arm64/beta ./cmd/beta"
has_call "darwin gets the Makefile flags" "darwin/amd64 cgo=0 build -trimpath -ldflags -s -w -X example.test/demo/v.v=v1.2.3 -o $work/out/darwin-amd64/alpha ./cmd/alpha"
has_call "freebsd defaults to the folders of its packages" "freebsd/amd64 cgo=0 build -trimpath -ldflags -s -w -X example.test/demo/v.v=v1.2.3 -X example.test/demo/config.defaultConfigDir=/usr/local/etc/splicefs/demo -X example.test/demo/config.defaultCacheDir=/var/cache/splicefs -o $work/out/freebsd-amd64/alpha ./cmd/alpha"
lacks_call "only freebsd changes the default folders" "linux/amd64 cgo=0 build -trimpath -ldflags -s -w -X example.test/demo/v.v=v1.2.3 -X example.test/demo/config"

# Without LDFLAGS the script must stop, not build programs that report no version.
status=0
message="$(cd "$tree" && PATH="$work/bin:$PATH" VERSION=v1.2.3 PACKAGE_DATE=20261112 \
    sh scripts/build-all.sh "$work/out2" 2>&1)" || status=$?
if [ "$status" -ne 0 ] && printf '%s' "$message" | grep -q 'LDFLAGS is not set'; then
    echo "ok   refuses to run without LDFLAGS"
else
    echo "FAIL runs without LDFLAGS (status $status): $message"
    failed=$((failed + 1))
fi

[ "$failed" -eq 0 ] || { echo "$failed failed"; exit 1; }
echo "build-all: every program, system and CPU type as expected"
