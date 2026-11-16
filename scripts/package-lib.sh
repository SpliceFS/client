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
