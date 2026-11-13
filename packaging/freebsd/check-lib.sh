# Sourced by scripts/freebsd-job.sh inside the FreeBSD machine. It says what "the service
# works" means for this program; the rest of the job is the same for every SpliceFS program.

# The client runs as root, as on Linux: mounting needs it.
expected_user=root

# Status 0 when the running client answers the status command over its control socket. A
# freshly installed client has no clusters and no mounts, and must still be there to ask.
answers() {
    /usr/local/bin/splicefs status >/dev/null 2>&1
}
