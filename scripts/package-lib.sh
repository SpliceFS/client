# Sourced by the package scripts. Says what is being packaged and what its version is
# called in each package format.
: "${VERSION:?VERSION is not set}"
: "${PACKAGE_DATE:?PACKAGE_DATE is not set}"

name="$(sed -n 's/^Package: //p' packaging/deb/control)"
program="$(basename cmd/*)"
confdir="/etc/splicefs/${name#splicefs-}"

# VERSION is what git describe prints: v1.2.3 on a release, v1.2.3-4-gabc after it, and
# only a commit when there is no release yet. base is the release number, extra the rest.
case "$VERSION" in
    v[0-9]*)
        base="${VERSION#v}"
        extra="${base#*-}"
        if [ "$extra" = "$base" ]; then extra=""; fi
        base="${base%%-*}"
        ;;
    *)
        base=0.0.0
        extra="$VERSION"
        ;;
esac

# Debian and rpm versions take the rest as a suffix: 1.2.3+4.gabc.
if [ -n "$extra" ]; then
    debver="$base+$(printf '%s' "$extra" | tr - .)"
else
    debver="$base"
fi
rpmver="$debver"

# An Alpine version allows digits and a fixed list of suffixes only, so a build that is
# not a release carries the date of its commit: 1.2.3_git20261112.
if [ -n "$extra" ]; then
    apkver="${base}_git$PACKAGE_DATE"
else
    apkver="$base"
fi

fail() {
    echo "FAIL: $*" >&2
    exit 1
}
