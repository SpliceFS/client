#!/bin/sh
# Uses the rpm repository the way a user would, with signature checking on: adds the public
# key and a repository file, installs and removes the package, then proves signatures are
# enforced by pointing the package manager at a damaged copy. It runs inside a throwaway
# container of any rpm based system (make repo-test): dnf on Rocky Linux and Amazon Linux,
# zypper on openSUSE. The repository is at /repo and the public keys are at /keys.
set -eu

. scripts/package-lib.sh
key=/etc/pki/rpm-gpg/RPM-GPG-KEY-splicefs

install -d -m 755 /etc/pki/rpm-gpg
install -m 644 /keys/splicefs.asc "$key"

if command -v dnf >/dev/null 2>&1; then
    repofile=/etc/yum.repos.d/splicefs.repo
    cat > "$repofile" <<REPO
[$name]
name=SpliceFS ${name#splicefs-}
baseurl=file:///repo/rpm/\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-splicefs
REPO
    # The exact version is named because the repository may hold older builds.
    pm_install() { dnf install -y "$name-$rpmver"; }
    pm_clean() { dnf clean all >/dev/null 2>&1; }
else
    repofile=/etc/zypp/repos.d/splicefs.repo
    rpm --import "$key"
    install -d -m 755 /etc/zypp/repos.d
    cat > "$repofile" <<REPO
[$name]
name=SpliceFS ${name#splicefs-}
baseurl=file:///repo/rpm/\$basearch
enabled=1
autorefresh=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-splicefs
REPO
    zypper --non-interactive refresh "$name" >/tmp/refresh.log 2>&1 || {
        cat /tmp/refresh.log
        fail "zypper refresh failed on the good repository"
    }
    pm_install() { zypper --non-interactive install "$name=$rpmver"; }
    pm_clean() { zypper --non-interactive clean --all >/dev/null 2>&1; }
fi

# The package manager's status is read after the call, never in a condition: a function in a
# condition runs with set -e off.
set +e
pm_install >/tmp/install.log 2>&1
status=$?
set -e
[ "$status" -eq 0 ] || {
    cat /tmp/install.log
    fail "install from the repository failed"
}
"$program" --version | grep -qF "$VERSION" || fail "$program --version does not report $VERSION"
rpm -e "$name" >/dev/null 2>&1 || fail "remove failed"
[ ! -e "/usr/bin/$program" ] || fail "remove left the program behind"

# /repo is read-only, so the damaged copy is private. repomd.xml is what the index
# signature covers; one more line in it makes the signature wrong.
cp -r /repo /tmp/bad-repo
for xml in /tmp/bad-repo/rpm/*/repodata/repomd.xml; do
    echo "<!-- damaged -->" >> "$xml"
done
sed -i 's#file:///repo/rpm#file:///tmp/bad-repo/rpm#' "$repofile"
pm_clean
set +e
pm_install >/tmp/bad.log 2>&1
status=$?
set -e
if [ "$status" -eq 0 ]; then
    cat /tmp/bad.log
    fail "the install accepted a repository with a broken signature"
fi
refusal='Bad (GPG|PGP) signature|Signature verification failed|checksum'
grep -Eqi "$refusal" /tmp/bad.log || {
    cat /tmp/bad.log
    fail "the install failed, but not with a signature error"
}
refused="$(grep -Eim1 "$refusal" /tmp/bad.log)"
echo "damaged repository refused: $refused"

# shellcheck source=/dev/null
. /etc/os-release
# PRETTY_NAME comes from /etc/os-release, which ShellCheck cannot read here.
# shellcheck disable=SC2154
echo "repository test passed on $PRETTY_NAME"
