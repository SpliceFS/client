#!/bin/sh
# Debian and Ubuntu (systemd) functions of scripts/service-test.sh; the file is sourced, so
# it runs nothing itself. $name, $debver and $program come from scripts/package-lib.sh and
# $platform from the harness.
# shellcheck disable=SC2154 # those variables are set by the file that sources this one

unit="$program.service"

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

# Reads the owner of the running process from /proc, which every system has, whatever ps it ships.
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
