#!/bin/sh
# Alpine (OpenRC) functions of scripts/service-test.sh; the file is sourced, so it runs
# nothing itself. $name, $apkver and $program come from scripts/package-lib.sh and $platform
# from the harness.
# shellcheck disable=SC2154 # those variables are set by the file that sources this one

# The images ship without a service manager. The distribution's own openrc package is the
# only thing installed. A container cannot reach the hardware that OpenRC's boot services
# expect, so OpenRC is told it runs in a container; that only configures the test system.
# The commands run on every boot of the container, so each one can run twice without harm.
init_command() {
    echo '[ -x /sbin/openrc ] || apk add -q openrc
    sed -i "/^#*rc_sys=/d" /etc/rc.conf
    echo "rc_sys=docker" >>/etc/rc.conf
    exec /sbin/init'
}

# busybox init powers off on SIGUSR2 after running the shutdown entries of inittab, which
# is how OpenRC stops its services; its default signal, SIGTERM, reboots.
stop_signal() {
    echo SIGUSR2
}

# Booted means OpenRC has reached the default runlevel, which is where the package adds the
# service. Before OpenRC is installed rc-status does not exist, and every wait must have a
# limit, so this polls.
wait_ready() {
    waited=0
    while :; do
        level="$(docker exec "$1" rc-status --runlevel 2>/dev/null || true)"
        [ "$level" != default ] || return 0
        if [ "$(docker inspect -f '{{.State.Running}}' "$1")" != true ]; then
            echo "the container stopped before OpenRC was up:" >&2
            docker logs --tail 30 "$1" >&2 || true
            return 1
        fi
        waited=$((waited + 1))
        if [ "$waited" -ge 90 ]; then
            echo "OpenRC is in runlevel '$level' after 90 seconds" >&2
            return 1
        fi
        sleep 1
    done
}

install_package() {
    case "$platform" in
        linux/arm64) arch=aarch64 ;;
        *) arch=x86_64 ;;
    esac
    pkg="/pkg/alpine/$arch/$name-$apkver-r0.apk"
    docker exec "$1" test -f "$pkg" || fail "$pkg does not exist; run make package first"
    # The build key is a throwaway, so the signature cannot be checked here.
    docker exec "$1" apk add -q --allow-untrusted "$pkg" >/dev/null
}

remove_package() {
    docker exec "$1" apk del -q "$name" >/dev/null
}

service_active() {
    docker exec "$1" rc-service "$program" status >/dev/null 2>&1
}

service_enabled() {
    docker exec "$1" rc-update show default | grep -q "^ *$program "
}

service_restart() {
    docker exec "$1" rc-service "$program" restart >/dev/null
}

# The main process is the one that runs the program, not supervise-daemon, whose command
# line only mentions it. The owner is read from /proc because the images have no ps.
service_user() {
    docker exec "$1" sh -c '
        for dir in /proc/[0-9]*; do
            [ "$(readlink "$dir/exe" 2>/dev/null)" = "/usr/bin/$1" ] || continue
            uid="$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" "$dir/status")"
            getent passwd "$uid" | cut -d: -f1
            exit 0
        done
        exit 1' sh "$program"
}

service_report() {
    docker exec "$1" rc-service "$program" status || true
    docker exec "$1" tail -n 30 "/var/log/${program}.log" || true
    docker logs --tail 10 "$1" || true
}

# Prints why an image cannot be tested, or nothing when it can.
skip_reason() {
    :
}
