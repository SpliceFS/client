#!/bin/sh
# Debian and Ubuntu (systemd) functions of scripts/service-test.sh; the file is sourced, so
# it runs nothing itself. $name, $debver and $program come from scripts/package-lib.sh and
# $platform from the harness.
# shellcheck disable=SC2154 # those variables are set by the file that sources this one

unit="$program.service"

# The images ship without a service manager. The distribution's own systemd package is the
# only thing installed; recommended packages are left out because systemd runs without them.
init_command() {
    echo '[ -x /lib/systemd/systemd ] || {
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq && apt-get install -y -qq --no-install-recommends systemd
    } && exec /lib/systemd/systemd'
}

# systemd shuts down cleanly on this signal; docker stop's default would only kill it.
stop_signal() {
    echo SIGRTMIN+3
}

# Polls instead of using is-system-running --wait because the command cannot run at all
# until systemd is process 1, and every wait must have a limit. Units that cannot work in a
# container make the state degraded, which is fine.
wait_ready() {
    waited=0
    while :; do
        state="$(docker exec "$1" systemctl is-system-running 2>/dev/null || true)"
        case "$state" in running | degraded) return 0 ;; esac
        if [ "$(docker inspect -f '{{.State.Running}}' "$1")" != true ]; then
            echo "the container stopped before systemd was up:" >&2
            docker logs --tail 30 "$1" >&2 || true
            return 1
        fi
        waited=$((waited + 1))
        if [ "$waited" -ge 90 ]; then
            echo "systemd still reports '$state' after 90 seconds" >&2
            return 1
        fi
        sleep 1
    done
}

install_package() {
    pkg="/pkg/${name}_${debver}_${platform#linux/}.deb"
    docker exec "$1" test -f "$pkg" || fail "$pkg does not exist; run make package first"
    # apt resolves the dependencies of a local file; dpkg -i alone would not.
    docker exec -e DEBIAN_FRONTEND=noninteractive "$1" sh -c \
        "apt-get update -qq && apt-get install -y -qq $pkg" >/dev/null
}

remove_package() {
    docker exec -e DEBIAN_FRONTEND=noninteractive "$1" apt-get remove -y -qq "$name" >/dev/null
}

service_active() {
    docker exec "$1" systemctl is-active --quiet "$unit"
}

service_enabled() {
    docker exec "$1" systemctl is-enabled --quiet "$unit"
}

service_restart() {
    docker exec "$1" systemctl restart "$unit"
}

# Reads the owner of the running process from /proc because the images have no ps.
service_user() {
    docker exec "$1" sh -c '
        pid="$(systemctl show -p MainPID --value "$1")"
        [ "${pid:-0}" -gt 0 ] || exit 1
        uid="$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" "/proc/$pid/status")"
        getent passwd "$uid" | cut -d: -f1' sh "$unit"
}

service_report() {
    docker exec "$1" systemctl status --no-pager --full "$unit" || true
    docker exec "$1" journalctl -u "$unit" --no-pager -n 30 || true
    docker logs --tail 10 "$1" || true
}

# Prints why an image cannot be tested, or nothing when it can.
skip_reason() {
    :
}
