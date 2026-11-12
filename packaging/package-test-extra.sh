# Sourced by scripts/package-test-<format>.sh after the package is installed.
# mount(8) looks for its helpers in /sbin, which on some systems is a link to /usr/sbin and
# on others a folder of its own. Either way the helper must be found there and must be
# the client: asked for nothing, it answers with its usage and status 1.
[ -x /sbin/mount.splicefs ] || fail "/sbin/mount.splicefs is missing"
if /sbin/mount.splicefs >/dev/null 2>&1; then fail "mount.splicefs without arguments succeeded"; fi
/sbin/mount.splicefs 2>&1 | grep -q "mount.splicefs" || fail "mount.splicefs does not print its usage"
