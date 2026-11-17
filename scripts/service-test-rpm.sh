#!/bin/sh
# Rocky Linux, Amazon Linux and openSUSE (systemd) functions of scripts/service-test.sh; the
# file is sourced, so it runs nothing itself. $name, $rpmver and $program come from
# scripts/package-lib.sh and $platform from the harness.
# shellcheck disable=SC2154 # those variables are set by the file that sources this one

unit="$program.service"

install_package() {
    # The file name carries the CPU type in rpm's own spelling.
    case "$platform" in
        linux/arm64) arch=aarch64 ;;
        linux/amd64) arch=x86_64 ;;
        *) fail "no rpm for platform $platform" ;;
    esac
    pkg="/pkg/$name-$rpmver-1.$arch.rpm"
    docker exec "$1" test -f "$pkg" || fail "$pkg does not exist; run make package first"
    # The system's package manager resolves the dependencies of a local file; rpm -i alone
    # would not. The build key is a throwaway, so zypper must be told to accept an unsigned
    # file.
    docker exec "$1" sh -c '
        if command -v dnf >/dev/null 2>&1; then
            dnf install -y -q "$1"
        else
            zypper --non-interactive install --allow-unsigned-rpm "$1"
        fi' sh "$pkg" >/dev/null
}

remove_package() {
    docker exec "$1" sh -c '
        if command -v dnf >/dev/null 2>&1; then
            dnf remove -y -q "$1"
        else
            zypper --non-interactive remove "$1"
        fi' sh "$name" >/dev/null
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
