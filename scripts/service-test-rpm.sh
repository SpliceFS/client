#!/bin/sh
# Rocky Linux, Amazon Linux and openSUSE (systemd) functions of scripts/service-test.sh; the
# file is sourced, so it runs nothing itself. $name, $rpmver and $program come from
# scripts/package-lib.sh and $platform from the harness.
# shellcheck disable=SC2154 # those variables are set by the file that sources this one

unit="$program.service"

# Rocky Linux 8 ships systemd; the other images do not. The distribution's own systemd
# package is the only thing installed, without the packages it only recommends, because
# systemd runs without them. dnf and zypper are told apart on the system itself, as
# scripts/package-test-rpm.sh does.
init_command() {
    echo '[ -x /usr/lib/systemd/systemd ] || {
        if command -v dnf >/dev/null 2>&1; then
            dnf install -y -q --setopt=install_weak_deps=False systemd
        else
            zypper --non-interactive --gpg-auto-import-keys install --no-recommends systemd
        fi
    } && exec /usr/lib/systemd/systemd'
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
    case "$1" in
        # Its repository signing key expired in June 2026, after the release reached its end
        # of life, so zypper refuses to install systemd from it.
        opensuse/leap:15.6) echo "openSUSE Leap 15.6 is past its end of life: its own systemd can no longer be installed" ;;
    esac
}
