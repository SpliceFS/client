# shellcheck shell=sh
# answers_text is read by scripts/service-test.sh, which sources this file.
# shellcheck disable=SC2034
# Sourced by scripts/service-test.sh. It says what "the service works" means for this
# program; everything else in the service test is the same for every SpliceFS program.

# The client listens on no network port. A person asks it for its state with the status
# command, which talks to the running service over its control socket (docs/configuration.md).
# A freshly installed client has no clusters and no mounts, and must still answer.
answers_text="answers the status command, run inside the container"

# The unit has no User= line on purpose: the control socket is readable only by the user
# the client runs as, and mount(8) helpers run as root, so the service runs as root.
# program is set by scripts/service-test.sh before it sources this file.
# shellcheck disable=SC2154
expected_user="$(sed -n 's/^User=//p' "packaging/systemd/$program.service")"
expected_user="${expected_user:-root}"

# The client publishes no port, so docker run needs no extra options.
run_options() {
    :
}

# Status 0 when "splicefs status" succeeds against the running service. The command exits
# with an error when it cannot reach the control socket, so only a running service passes.
answers() {
    docker exec "$1" "$program" status >/dev/null 2>&1
}
