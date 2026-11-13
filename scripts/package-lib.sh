# Sourced by the package scripts. Says what is being packaged and what its version is
# called in each package format.
: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"

name="$(sed -n 's/^Package: //p' packaging/deb/control)"
program="$(basename cmd/*)"
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

# VERSION is what git describe prints: v1.2.3 on a release, v1.2.3-rc.1 on a pre-release,
# v1.2.3-4-gabc after either, only a commit when there is no release yet, and a trailing
# -dirty when the tree has changes. Split it into core (1.2.3), pre (rc.1, may be empty) and
# extra (4.gabc, plus dirty), which is everything that makes a build something other than
# the tagged release.
tag="$VERSION"
dirty=""
case "$tag" in
    *-dirty) dirty=dirty; tag="${tag%-dirty}" ;;
esac
core=0.0.0
pre=""
extra=""
case "$tag" in
    v[0-9]*)
        commits="$(printf '%s' "$tag" | sed -n 's/^.*-\([0-9][0-9]*-g[0-9a-f][0-9a-f]*\)$/\1/p')"
        if [ -n "$commits" ]; then
            tag="${tag%-"$commits"}"
            extra="$(printf '%s' "$commits" | tr - .)"
        fi
        core="${tag#v}"
        core="${core%%-*}"
        if [ "$core" != "${tag#v}" ]; then pre="${tag#v"$core"-}"; fi
        printf '%s' "$core" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
            fail "tag $tag is not vMAJOR.MINOR.PATCH[-pre-release]"
        # Alpine takes only these pre-release words, so nothing else can be published.
        if [ -n "$pre" ]; then
            printf '%s' "$pre" | grep -Eq '^(alpha|beta|pre|rc)(\.?[0-9]+)?$' ||
                fail "tag $tag: pre-release $pre must be alpha, beta, pre or rc, then a number"
        fi
        ;;
    *)
        extra="$tag"
        ;;
esac
if [ -n "$dirty" ]; then extra="${extra:+$extra.}dirty"; fi

# Debian and rpm: a tilde sorts before everything, so 1.2.3~rc.1 is older than 1.2.3, and
# a plus suffix sorts after, so 1.2.3+4.gabc is newer than the release it follows.
debver="$core"
if [ -n "$pre" ]; then debver="$debver~$pre"; fi
if [ -n "$extra" ]; then debver="$debver+$extra"; fi
rpmver="$debver"

# An Alpine version allows digits and a fixed list of suffixes only, so the pre-release
# becomes _rc1 and a build that is not exactly a tag carries the date of its commit:
# 1.2.3_rc1_git20261112.
apkver="$core"
if [ -n "$pre" ]; then apkver="${apkver}_$(printf '%s' "$pre" | tr -d .)"; fi
if [ -n "$extra" ]; then apkver="${apkver}_git$PACKAGE_DATE"; fi

# FreeBSD compares versions field by field and knows rc and friends as older than the release,
# so the pre-release becomes one more field (1.2.3.rc1) and a build that is not exactly a tag
# carries the date of its commit as the last field: 1.2.3.20261112.
pkgver="$core"
if [ -n "$pre" ]; then pkgver="$pkgver.$(printf '%s' "$pre" | tr -d .)"; fi
if [ -n "$extra" ]; then pkgver="$pkgver.$PACKAGE_DATE"; fi
