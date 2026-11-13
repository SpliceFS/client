#!/bin/sh
# Proves the service, not only the files: on every system in packaging/test-systems it starts
# one container whose process 1 is that system's own service manager, installs the package
# into the running system, and checks that the service starts by itself, runs as its own
# user, does its job, restarts, comes back after a reboot and stops
# when the package is removed. A failing system does not stop the run, so one run shows
# every system that is broken. Runs on the host (make service-test, CI); it needs Docker and
# curl. Environment: VERSION and PACKAGE_DATE as for the build; PLATFORM (linux/amd64 or
# linux/arm64) tests that CPU type by emulation, empty means this machine's own; SYSTEMS
# limits the run to the lines of packaging/test-systems that contain that text.
#
# What differs between package formats lives in scripts/service-test-<format>.sh, which this
# file sources and which defines these functions (the first argument is the container):
#   init_command             the shell command that becomes process 1: it installs the
#                            distribution's own service manager when the image lacks it
#   stop_signal              the signal that makes the service manager shut down cleanly
#   wait_ready C             returns once the service manager has finished booting
#   install_package C        installs the package from /pkg with the native package manager
#   remove_package C         removes it again
#   service_active C         status 0 when the service manager says the service is running
#   service_enabled C        status 0 when the service starts at boot
#   service_restart C        restarts the service through the service manager
#   service_user C           prints the user the main process of the service runs as
#   service_report C         prints the service's status and its last 30 log lines
#   skip_reason IMAGE        prints why this image cannot be tested, or nothing when it can
set -eu

: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
. scripts/package-lib.sh

# What "the service works" means differs per program, so it lives with the packaging:
# expected_user, run_options (extra docker run options), answers C (status 0 when the
# service does its job) and answers_text (the words for the report).
. packaging/service-check-lib.sh

# Named explicitly because after a run with another platform the local image tag can point
# at the other CPU type, and an empty setting would then silently test the wrong one.
platform="${PLATFORM:-$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')}"
log="$(mktemp)"
container=""
cleanup() {
    rm -f "$log"
    if [ -n "$container" ]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Runs the command once a second until it succeeds or the given number of seconds is over.
wait_for() {
    limit="$1"
    shift
    waited=0
    until "$@"; do
        waited=$((waited + 1))
        [ "$waited" -lt "$limit" ] || return 1
        sleep 1
    done
}

# Ends this system's run with the message when the command fails. The command runs through
# "$@" because errexit is off in every check (see run_checks): each one is guarded
# explicitly, and its status is the answer.
must() {
    message="$1"
    shift
    "$@" || fail "$message"
}

# The same for a command that has to fail: ends the run with the message when it succeeds.
refuse() {
    message="$1"
    shift
    if "$@"; then fail "$message"; fi
}

active_and_answering() {
    service_active "$1" && answers "$1"
}

inactive() {
    ! service_active "$1"
}

check_starts_by_itself() {
    service_enabled "$1" || fail "the service is not enabled after the install"
    must "the service is not active 20 seconds after the install" wait_for 20 service_active "$1"
    echo "ok: enabled and active after the install, nobody started it"
}

check_user() {
    user="$(service_user "$1")" || fail "cannot find the main process of the service"
    [ "$user" = "$expected_user" ] ||
        fail "the service runs as '$user', not as '$expected_user'"
    echo "ok: runs as $user"
}

check_answers() {
    must "the service does not do its job: expected that it $answers_text" wait_for 10 answers "$1"
    echo "ok: $answers_text"
}

check_restart() {
    service_restart "$1" || fail "the restart failed"
    must "the service is not active and answering 20 seconds after a restart" \
        wait_for 20 active_and_answering "$1"
    echo "ok: active and answering after a restart"
}

check_reboot() {
    docker restart "$1" >/dev/null || fail "docker restart failed"
    must "the service is not active and answering 60 seconds after a reboot" \
        wait_for 60 active_and_answering "$1"
    echo "ok: active and answering after a reboot, nobody started it"
}

check_removal() {
    remove_package "$1" || fail "removing the package failed"
    must "the service is still active after the package was removed" wait_for 10 inactive "$1"
    refuse "the service still answers after the package was removed" answers "$1"
    wait_ready "$1" || fail "the service manager is not healthy after the package was removed"
    echo "ok: stopped and silent after the removal, the service manager is healthy"
}

# One system. Every command is guarded with fail because errexit is off for anything called
# from an if; the caller runs this in a subshell so that fail ends only this system.
run_checks() {
    wait_ready "$1" || fail "the service manager did not finish booting"
    install_package "$1" || fail "installing the package failed"
    check_starts_by_itself "$1"
    check_user "$1"
    check_answers "$1"
    check_restart "$1"
    check_reboot "$1"
    check_removal "$1"
}

passed=0
failed=0
count=0
systems="$(cat packaging/test-systems)"

# A here-document keeps the counters in this shell; a pipe would run the loop in a subshell.
while read -r format image; do
    # Blank lines and comments are skipped; every other line is a system to test.
    case "$format" in "" | "#"*) continue ;; *) ;; esac
    case "$format $image" in *"${SYSTEMS:-}"*) ;; *) continue ;; esac
    if [ ! -f "scripts/service-test-$format.sh" ]; then
        echo "skipped: no service test for $format yet ($image)"
        continue
    fi
    # shellcheck source=/dev/null
    . "scripts/service-test-$format.sh"
    reason="$(skip_reason "$image")"
    if [ -n "$reason" ]; then
        echo "skipped: $format $image: $reason"
        continue
    fi

    count=$((count + 1))
    container="splicefs-service-test-$$-$count"
    started=0
    signal="$(stop_signal)"
    options="$(run_options)"
    init="$(init_command)"
    # run_options prints whole options and nothing else, so it is left unquoted on purpose.
    # shellcheck disable=SC2086
    if docker run -d --name "$container" --platform "$platform" \
        --privileged --cgroupns=host \
        --tmpfs /run --tmpfs /run/lock --tmpfs /tmp -e container=docker \
        --stop-signal "$signal" \
        -v "$root/dist":/pkg:ro $options \
        "$image" sh -c "$init" >/dev/null 2>"$log"; then
        started=1
    fi

    # Errexit is switched off here on purpose: every command of run_checks is guarded with
    # fail, and a failing check must end only this system, not the whole run.
    checked=1
    if [ "$started" -eq 1 ]; then
        set +e
        (run_checks "$container") >>"$log" 2>&1
        checked=$?
        set -e
    fi
    if [ "$checked" -eq 0 ]; then
        echo "ok   $format $image"
        passed=$((passed + 1))
    else
        echo "FAIL $format $image"
        {
            echo "--- $program.service on $image"
            service_report "$container" || true
        } >>"$log" 2>&1
        tail -n 70 "$log" | sed 's/^/     /'
        failed=$((failed + 1))
    fi
    docker rm -f "$container" >/dev/null 2>&1 || true
    container=""
done <<LIST
$systems
LIST

echo "passed: $passed  failed: $failed"
[ "$failed" -eq 0 ]
