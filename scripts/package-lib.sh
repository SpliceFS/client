# shellcheck shell=sh
# The scripts that source this file read these variables; this file reads none of them.
# shellcheck disable=SC2034
# Sourced by the package scripts. Says what is being packaged and what its version is
# called in each package format.
: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"

name="$(sed -n 's/^Package: //p' packaging/deb/control)"
# Every program in cmd/ is installed. The one with a service unit is the service that the
# package starts; any other is a tool for people, which talks to that service.
programs=""
for cmd_folder in cmd/*; do programs="$programs $(basename "$cmd_folder")"; done
programs="${programs# }"
program=""
for candidate in $programs; do
    if [ -f "packaging/systemd/$candidate.service" ]; then program="$candidate"; fi
done
[ -n "$program" ] || { echo "FAIL: no program in cmd/ has a unit in packaging/systemd" >&2; exit 1; }
confdir="/etc/splicefs/${name#splicefs-}"
# One folder name per line; see the file for why the package ships them empty.
confsubdirs="$(grep -v -e '^#' -e '^$' packaging/config-folders)"
# Links the package installs, "<link> <target>" per line; a package without the file has none.
links=""
if [ -f packaging/links ]; then links="$(grep -v -e '^#' -e '^$' packaging/links)"; fi

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# Writes the service unit as the packages ship it to the file $1. The unit in the repository
# is written for a manual install under /usr/local; the packages put the program in /usr/bin.
package_unit() {
    sed 's|/usr/local/bin/|/usr/bin/|' "packaging/systemd/$program.service" > "$1"
}

# The file name of the plain program archive (archive-programs.sh) of the system $1, written
# <os>-<arch>. The version is the one the Makefile made, without the v.
archive_name() {
    printf '%s-%s-%s.tar.gz\n' "$name" "${VERSION#v}" "$1"
}

# Prints the systems make build-all put in the folder $1/bin, one <os>-<arch> to a line. The
# build decides what the release is made for, so the files that are made from its output ask
# it here and keep no list of their own.
built_systems() {
    [ -d "$1/bin" ] || fail "missing $1/bin, run make build-all first"
    found="$(cd "$1/bin" && LC_ALL=C ls)"
    [ -n "$found" ] || fail "$1/bin holds no system, run make build-all first"
    echo "$found"
}

# Fetches an image for one test system: up to 3 tries, 5 seconds apart, quiet on success.
# A registry that fails to deliver now and then is not a fault of the package under test, so
# the callers report a failed download as one. Pulling first also keeps the download out of
# the log of the test itself. Usage: pull_image PLATFORM IMAGE; status 1 after the last try.
pull_image() {
    tries=0
    while [ "$tries" -lt 3 ]; do
        tries=$((tries + 1))
        if docker pull --quiet --platform "$1" "$2" >/dev/null 2>&1; then return 0; fi
        if [ "$tries" -lt 3 ]; then sleep 5; fi
    done
    return 1
}

# Whether a line of packaging/test-systems is part of this run. ONLY names one format or one
# image; empty means every line. Every script that walks the list asks here, so that CI can
# run one format per job the same way each of them. Usage: system_selected FORMAT IMAGE.
system_selected() {
    [ -z "${ONLY:-}" ] || [ "$ONLY" = "$1" ] || [ "$ONLY" = "$2" ]
}

# Starts a server image as a container the way the images README boots one: the service
# manager is process 1. The image sets the rest (the container variable and the stop signal).
# The Alpine images need none of these options and are not harmed by them, so every system
# is started the same way. The arguments are the rest of the docker run command: name,
# platform, mounts, and the image last. Prints nothing on success.
boot_system() {
    docker run -d --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock --tmpfs /tmp "$@" >/dev/null
}

# Status 0 when the service manager of the container $2 has finished booting. For systemd that
# is what is-system-running says; units that cannot work in a container make its state
# degraded, which is fine. OpenRC (the apk format) is up once it reaches the default
# runlevel, where the package adds the service. Usage: service_manager_up FORMAT CONTAINER.
service_manager_up() {
    case "$1" in
        apk)
            level="$(docker exec "$2" rc-status --runlevel 2>/dev/null)" || true
            [ "$level" = default ]
            ;;
        *)
            state="$(docker exec "$2" systemctl is-system-running 2>/dev/null)" || true
            # Any other state means systemd is not up yet.
            case "$state" in running | degraded) return 0 ;; *) return 1 ;; esac
            ;;
    esac
}

# Returns once the service manager of the container $2 has booted, so that a test never starts
# on a system that is still coming up. It polls because the first commands cannot run until
# the manager is process 1, and every wait has a limit: it ends with status 1 when the
# container stops or when the manager is not up after 90 seconds.
# Usage: wait_booted FORMAT CONTAINER.
wait_booted() {
    waited=0
    # service_manager_up is one test whose status is the answer, so set -e being off inside it changes nothing.
    # shellcheck disable=SC2310
    until service_manager_up "$1" "$2"; do
        # A container that cannot be inspected is counted as stopped.
        running="$(docker inspect -f '{{.State.Running}}' "$2")" || running=""
        if [ "$running" != true ]; then
            echo "the container stopped before its service manager was up:" >&2
            docker logs --tail 30 "$2" >&2 || true
            return 1
        fi
        waited=$((waited + 1))
        if [ "$waited" -ge 90 ]; then
            echo "the service manager is not up after 90 seconds" >&2
            return 1
        fi
        sleep 1
    done
}

# VERSION is the version in the file VERSION as the Makefile makes it: v1.2.3 for a release,
# v1.2.3-rc.1 for a pre-release, and a trailing -dirty when the tree has uncommitted changes.
# Split it into core (1.2.3), pre (rc.1, may be empty) and extra (dirty, or empty), which is
# what makes a build something other than the released version. Anything else is refused: a
# version that is not in this form would be packaged under a number that sorts wrongly.
release="${VERSION%-dirty}"
extra=""
if [ "$release" != "$VERSION" ]; then extra=dirty; fi
printf '%s' "$release" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$' ||
    fail "version $VERSION is not vMAJOR.MINOR.PATCH[-pre-release]"
core="${release#v}"
core="${core%%-*}"
pre=""
if [ "$core" != "${release#v}" ]; then pre="${release#v"$core"-}"; fi
# Alpine takes only these pre-release words, so nothing else can be published.
if [ -n "$pre" ]; then
    printf '%s' "$pre" | grep -Eq '^(alpha|beta|pre|rc)(\.?[0-9]+)?$' ||
        fail "version $VERSION: pre-release $pre must be alpha, beta, pre or rc, then a number"
fi

# Debian and rpm: a tilde sorts before everything, so 1.2.3~rc.1 is older than 1.2.3, and
# a plus suffix sorts after, so 1.2.3+dirty is newer than the release it follows.
debver="$core"
if [ -n "$pre" ]; then debver="$debver~$pre"; fi
if [ -n "$extra" ]; then debver="$debver+$extra"; fi
rpmver="$debver"

# An Alpine version allows digits and a fixed list of suffixes only, so the pre-release
# becomes _rc1 and a build that is not exactly the release carries the date of its
# commit: 1.2.3_rc1_git20261112.
apkver="$core"
if [ -n "$pre" ]; then apkver="${apkver}_$(printf '%s' "$pre" | tr -d .)"; fi
if [ -n "$extra" ]; then apkver="${apkver}_git$PACKAGE_DATE"; fi

# FreeBSD compares versions field by field and knows rc and friends as older than the release,
# so the pre-release becomes one more field (1.2.3.rc1) and a build that is not exactly the
# release carries the date of its commit as the last field: 1.2.3.20261112.
pkgver="$core"
if [ -n "$pre" ]; then pkgver="$pkgver.$(printf '%s' "$pre" | tr -d .)"; fi
if [ -n "$extra" ]; then pkgver="$pkgver.$PACKAGE_DATE"; fi
