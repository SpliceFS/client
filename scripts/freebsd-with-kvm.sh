#!/bin/sh
# Runs a command only when the container can write to the hardware virtualization device. It
# runs as root inside a prepared FreeBSD image, in front of that image's machine runner
# (scripts/freebsd-lib.sh), which starts an emulated machine without a word when the device is
# not writable. Emulation takes many times as long, so it must be a failure here.
#
#   freebsd-with-kvm.sh <device> <command> [argument ...]
set -eu

if [ "$#" -lt 2 ]; then
    echo "usage: freebsd-with-kvm.sh DEVICE COMMAND [ARGUMENT ...]" >&2
    exit 2
fi
device="$1"
shift

if [ ! -c "$device" ] || [ ! -w "$device" ]; then
    echo "FAIL: $device is not a device this container can write to, so the FreeBSD machine would run in software emulation, which is far too slow. Run on Linux on amd64 with KVM and pass the device to docker with --device." >&2
    exit 1
fi
exec "$@"
