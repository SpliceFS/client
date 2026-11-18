#!/bin/sh
# Host test of the scripts around the FreeBSD machines: no Docker, no machine, no network.
# Docker is a stand-in program that records what it was asked to run, and the hardware
# virtualization device is a file of the test's own choosing.
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# Runs the command in a subshell, with its output in $work/output, and fails unless its status
# is $1 and its output contains $2. The command may call fail or exit, as the scripts do.
expect() {
    want_status="$1"
    want_text="$2"
    shift 2
    status=0
    ("$@") >"$work/output" 2>&1 || status=$?
    [ "$status" -eq "$want_status" ] ||
        { cat "$work/output" >&2; fail "[$*] ended with status $status, want $want_status"; }
    # An empty text is no condition on the output.
    [ -z "$want_text" ] || grep -qF -- "$want_text" "$work/output" ||
        { cat "$work/output" >&2; fail "[$*] did not say [$want_text]"; }
}

# --- freebsd-lib.sh ---------------------------------------------------------------------

export FREEBSD_IMAGE='registry.invalid/freebsd-@RELEASE@:t1'
# shellcheck source=freebsd-lib.sh
. "$root/scripts/freebsd-lib.sh"

# The image of a release is the template with the release in it.
image_of_15_1="$(freebsd_image 15.1)"
[ "$image_of_15_1" = "registry.invalid/freebsd-15.1:t1" ] || fail "the image of 15.1 is [$image_of_15_1]"

# Without the device the machine would fall back to emulation, which takes many times as long.
# The script is what make runs before it builds anything. The device here is a file of the
# test's own, because the real one exists on few computers.
require_kvm_with() {
    (cd "$root" && KVM_DEVICE="$1" sh scripts/freebsd-require-kvm.sh)
}
: >"$work/a-file"
expect 1 "FAIL: the FreeBSD machine needs hardware virtualization (KVM), and $work/no-kvm is not a device" \
    require_kvm_with "$work/no-kvm"
expect 1 "$work/a-file is not a device" require_kvm_with "$work/a-file"
expect 0 "" require_kvm_with /dev/null

out="$work/out"
mkdir -p "$work/freebsd-test" "$out"
printf '# The releases.\n\n14.5\n15.1\n' >"$work/freebsd-test/releases"
cd "$work"

calls=""
# A step that loops with a variable named like the lib's own: each. The lib must call its step
# for every release whatever names the step uses for its own variables (shell variables are
# global).
step() {
    for each in first second; do : "$each"; done
    calls="$calls $1"
}
freebsd_each_release step
[ "$calls" = " 14.5 15.1" ] || fail "the steps ran for [$calls], want [ 14.5 15.1]"

# A template without the place for the release would name one image for every release.
without_place_for_release() {
    FREEBSD_IMAGE=registry.invalid/freebsd:t1
    freebsd_each_release step
}
expect 1 "@RELEASE@" without_place_for_release

# FREEBSD limits the run to one release, and a release that is not listed is a mistake.
calls=""
FREEBSD=15.1
freebsd_each_release step
[ "$calls" = " 15.1" ] || fail "the steps ran for [$calls], want [ 15.1]"
FREEBSD=99.9
expect 1 "no release in freebsd-test/releases matches 99.9" freebsd_each_release step
unset FREEBSD

# The machine is started in the image of its release, with the device passed on and the folders
# named as the container sees them.
mkdir -p "$work/bin"
cat >"$work/bin/docker" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$STUB_ARGS"
exit "${STUB_STATUS:-0}"
EOF
chmod +x "$work/bin/docker"
export STUB_ARGS="$work/docker-args"
PATH="$work/bin:$PATH"

here="$(pwd)"
freebsd_run 15.1 "$out/job" "$out/results" "$out/in.tar"
[ "$run_status" -eq 0 ] || fail "run_status is $run_status after a good run"
cat >"$work/want-args" <<EOF
run
--rm
--user
0
--device
/dev/kvm
-e
VM_TIMEOUT=900
-v
$here:/src:ro
-v
$out:/out
registry.invalid/freebsd-15.1:t1
sh
/src/scripts/freebsd-with-kvm.sh
/dev/kvm
/freebsd/vm.sh
amd64
/freebsd/disk.qcow2
/out/job
/out/results
/out/in.tar
EOF
cmp -s "$work/want-args" "$STUB_ARGS" ||
    { diff "$work/want-args" "$STUB_ARGS" >&2 || true; fail "docker was asked for something else"; }

# Without an input archive the machine gets no fourth disk, and a failed run is reported by
# status and does not stop the caller.
STUB_STATUS=3 freebsd_run 14.5 "$out/job" "$out/results"
[ "$run_status" -eq 3 ] || fail "run_status is $run_status after a run that ended with 3"
last_argument="$(tail -n 1 "$STUB_ARGS")"
[ "$last_argument" = "/out/results" ] || fail "the last argument is [$last_argument], not the result folder"
echo "freebsd-lib: every release ran its step in its own image, and the machine needs the device"

# --- freebsd-with-kvm.sh ------------------------------------------------------------------

# The device in these cases is a file of the test's own too.

with_kvm="$root/scripts/freebsd-with-kvm.sh"
expect 2 "usage" sh "$with_kvm" /dev/null
expect 1 "FAIL: $work/no-kvm" sh "$with_kvm" "$work/no-kvm" echo ran
expect 1 "FAIL: $work/a-file" sh "$with_kvm" "$work/a-file" echo ran
! grep -q '^ran$' "$work/output" || fail "the command ran without the device"
expect 0 "ran with --one -two" sh "$with_kvm" /dev/null echo ran with --one -two
echo "freebsd-with-kvm: the machine is not started without the device"
