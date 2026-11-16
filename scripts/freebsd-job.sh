#!/bin/sh
# Runs inside the FreeBSD build machine as root, started by the first-boot script. It builds
# the packages with FreeBSD's own tools, installs the one for this machine and proves that the
# service runs. What "the service works" means for this program comes from check-lib.sh in
# the input folder (packaging/freebsd/check-lib.sh): expected_user and answers.
#
# It also builds the signed package repository. The input archive on the fourth disk holds
# keys/splicefs.rsa(.pub) and the packages published before (repo/FreeBSD:<major>:<cpu>/All/).
# The output gets repo/FreeBSD:<major>:<cpu>/ with the new package and the catalog of all
# packages that pkg repo signed. The package of this machine is installed from that repository with the signature
# check on, and a repository signed with another key must be refused.
#
#   freebsd-job.sh <input folder> <output folder> <package name> <program> <package version> <program version>
set -eu
in="$1"
out="$2"
name="$3"
program="$4"
version="$5"
program_version="$6"
mkdir -p "$out"

# Prints how long the phase that just ended took. timing.sh is not used: this script runs
# from the job folder on FreeBSD, where no other file of the repository is.
phase_start="$(date +%s)"
phase_done() {
    phase_now="$(date +%s)"
    echo "[time] $1: $((phase_now - phase_start))s"
    phase_start="$phase_now"
}

# Everything before this point, the boot and the first-boot setup, is the time since boot.
boot_sec="$(sysctl -n kern.boottime | sed -n 's/.*sec = \([0-9]*\).*/\1/p')"
if [ -n "$boot_sec" ]; then echo "[time] machine boot and first-boot setup: $((phase_start - boot_sec))s"; fi

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# shellcheck source=packaging/freebsd/check-lib.sh
. "$in/check-lib.sh"

major="$(freebsd-version -u | cut -d. -f1)"
machine="$(uname -m)"

# The base system ships only a stub that fetches the real pkg on first use.
pkg -N >/dev/null 2>&1 || env ASSUME_ALWAYS_YES=yes pkg bootstrap -y >/dev/null
phase_done "bootstrap pkg"

# The programs the package installs are the bin/ lines of plist, so that file is the one place
# that lists them for this job. The folders the package owns are its @dir lines.
programs="$(sed -n 's|^bin/||p' "$in/plist")"

# One package per CPU type. goarch is how the build names the binary, abiarch how pkg names
# the CPU type in an ABI string.
build() {
    goarch="$1"
    abiarch="$2"
    stage="/tmp/stage-$goarch"
    rm -rf "$stage"
    install -d "$stage/usr/local/bin" "$stage/usr/local/etc/rc.d"
    sed -n 's/^@dir //p' "$in/plist" | while read -r folder; do
        install -d "$stage/usr/local/$folder"
    done
    for each in $programs; do
        install -m 0755 "$in/$each-$goarch" "$stage/usr/local/bin/$each"
    done
    install -m 0755 "$in/$program.rc" "$stage/usr/local/etc/rc.d/$program"
    sed -e "s|@NAME@|$name|" -e "s|@PROGRAM@|$program|g" -e "s|@VERSION@|$version|" \
        -e "s|@ABI@|FreeBSD:$major:$abiarch|" "$in/manifest.in" > "/tmp/manifest-$goarch"
    mkdir -p "$out/FreeBSD-$major-$abiarch"
    osversion="$(uname -K)"
    pkg -o ABI="FreeBSD:$major:$abiarch" -o OSVERSION="$osversion" create \
        -M "/tmp/manifest-$goarch" -p "$in/plist" -r "$stage" -o "$out/FreeBSD-$major-$abiarch"
}
build amd64 amd64
phase_done "build the amd64 package"
build arm64 aarch64
phase_done "build the arm64 package"

case "$machine" in
    amd64) mine_abiarch=amd64 ;;
    arm64) mine_abiarch=aarch64 ;;
    *) fail "unknown machine $machine" ;;
esac
mine="$out/FreeBSD-$major-$mine_abiarch"
package="$mine/$name-$version.pkg"
[ -f "$package" ] || fail "pkg create did not write $package"
pkg info -F "$package" > "$out/info-$machine.txt"

# The signed repository. The input archive is a raw disk, so it is unpacked onto the
# filesystem first. pkg refuses a private key that others can read, and a file on the job
# disk cannot be given a mode, so the keys are copied to a folder of their own.
input_dir=/tmp/input
keys=/tmp/keys
mkdir -p "$input_dir" "$keys"
tar -xf /dev/vtbd3 -C "$input_dir" || fail "cannot read the input archive"
install -m 0600 "$input_dir/keys/splicefs.rsa" "$keys/splicefs.rsa"
install -m 0644 "$input_dir/keys/splicefs.rsa.pub" "$keys/splicefs.rsa.pub"
for abiarch in amd64 aarch64; do
    folder="$out/repo/FreeBSD:$major:$abiarch"
    mkdir -p "$folder/All"
    # Only the packages are taken over: the catalog is made again, so that it holds only what
    # this run wrote.
    if [ -d "$input_dir/repo/FreeBSD:$major:$abiarch/All" ]; then
        cp -R "$input_dir/repo/FreeBSD:$major:$abiarch/All/." "$folder/All/"
    fi
    cp "$out/FreeBSD-$major-$abiarch/$name-$version.pkg" "$folder/All/"
    # rsa: names the kind of key, as pkg 2 wants it; the catalog files then carry a signature.
    pkg repo "$folder" "rsa:$keys/splicefs.rsa" || fail "pkg repo could not sign the catalog of $folder"
    [ -f "$folder/meta.conf" ] || fail "pkg repo did not write $folder/meta.conf"
done
phase_done "unpack the input and sign the repositories"
repo_folder="$out/repo/FreeBSD:$major:$mine_abiarch"

# Writes a repository configuration: <file> <repository name> <folder>.
write_repo_conf() {
    printf '%s: {\n  url: "file://%s",\n  signature_type: "pubkey",\n  pubkey: "%s",\n  enabled: yes\n}\n' \
        "$2" "$3" "$keys/splicefs.rsa.pub" > "$1"
}

# Waits until the command given succeeds, at most $1 seconds. Sets wait_status to 0 when it
# did and to 1 when the time ran out, and never fails itself, so that the caller reports the
# timeout (a function used as a condition runs with set -e off).
wait_for() {
    limit="$1"
    shift
    n=0
    wait_status=0
    until "$@" >/dev/null 2>&1; do
        n=$((n + 1))
        if [ "$n" -ge "$limit" ]; then
            wait_status=1
            return 0
        fi
        sleep 1
    done
}
# What a person would look at first when the service misbehaves.
show() {
    service "$program" status || true
    ls -la "/var/run/$program" || true
    # pgrep prints no column header and no jail column, which are what a person looks for here.
    # shellcheck disable=SC2009
    ps axww -o pid,jid,user,command | grep -e "$program" -e '^ *PID' | grep -v grep || true
    tail -n 30 "/var/log/$program.log" 2>/dev/null || true
}

# The program says in its log what it did. Counting the lines before a signal lets a check
# look at what was written after it and nothing older.
service_log="/var/log/$program.log"

# Status 0 when the program logged, after the first $1 lines of its log, that it applied the
# configuration. A reload that failed logs "configuration not applied", which does not match.
reload_logged() {
    tail -n +"$(($1 + 1))" "$service_log" | grep -q 'configuration applied'
}

# Reloads the service and proves that the reload happened and that the service works after
# it. The signal is asynchronous and the old process keeps answering until the program acts
# on it, so an answer alone proves nothing: the reload is done when the program logs that it
# applied the configuration. The limit is only a safety stop for a reload that never happens.
reload_limit=20
reload_works() {
    lines_before="$(wc -l < "$service_log")"
    service "$program" reload || fail "reload failed"
    wait_for "$reload_limit" reload_logged "$lines_before"
    [ "$wait_status" -eq 0 ] || { show; fail "the service did not log that it applied the configuration after a reload"; }
    wait_for 10 answers
    [ "$wait_status" -eq 0 ] || { show; fail "the service does not do its job after a reload"; }
}

# The package is installed from the signed repository, as a person would, and from no other
# repository: -r names ours, so the public FreeBSD ones are not asked.
mkdir -p /usr/local/etc/pkg/repos
write_repo_conf /usr/local/etc/pkg/repos/splicefs.conf splicefs "$repo_folder"
pkg update -r splicefs
pkg install -y -r splicefs "$name"
phase_done "update the repository and install the package"
for each in $programs; do
    [ -x "/usr/local/bin/$each" ] || fail "$each is not installed"
    "/usr/local/bin/$each" --version | grep -qF "$program_version" ||
        fail "$each --version does not report $program_version"
done
# pkg ends every process an install script leaves behind, so on FreeBSD a package cannot
# start its service. What the package can and must do: the service is enabled without any
# setting, so it starts at boot, and one command starts it now.
service "$program" enabled || fail "the service is not enabled after the install"
service "$program" start || { show; fail "the service did not start"; }
wait_for 20 service "$program" status
[ "$wait_status" -eq 0 ] || { show; fail "the service is not running after its start"; }
wait_for 20 answers
[ "$wait_status" -eq 0 ] || { show; fail "the service does not do its job after its start"; }
service_pid="$(cat "/var/run/$program/$program.pid")"
owner="$(ps -o user= -p "$service_pid")"
[ "$owner" = "$expected_user" ] || fail "the service runs as '$owner', not as $expected_user"
reload_works
service "$program" restart >/dev/null || fail "restart failed"
wait_for 20 answers
[ "$wait_status" -eq 0 ] || { show; fail "the service does not do its job after a restart"; }
echo "installed, enabled, started with one command, running as $expected_user, answering, reload and restart work"
phase_done "start the service and check it"

# Removing the package stops the service and leaves nothing running.
pkg delete -y "$name" >/dev/null
# answers is one command whose status is the answer, so running it without set -e changes nothing.
# shellcheck disable=SC2310
if answers; then fail "the service still answers after the package was removed"; fi
for each in $programs; do
    [ ! -e "/usr/local/bin/$each" ] || fail "remove left $each behind"
done
echo "removed: the service stopped"
phase_done "remove the package"

# A repository that is not signed with our key is refused. Two copies of the repository are
# made, one signed again with a key made here and one not signed at all, and the package must
# not install from either while the configuration names our public key. pkg update reports
# success even when it throws a catalog away for its signature, so the install is what is
# tested. Each copy has its own name and its own configuration folder, so that it is the only
# repository pkg knows and the good catalog fetched above cannot stand in for it. The package
# was removed above, so an install has something to do.
( umask 077 && openssl genrsa -out /tmp/other.rsa 2048 2>/dev/null )
for kind in otherkey unsigned; do
    bad_folder="/tmp/bad-$kind"
    bad_conf="/tmp/bad-$kind-conf"
    rm -rf "$bad_folder" "$bad_conf"
    mkdir -p "$bad_folder" "$bad_conf"
    cp -R "$repo_folder/All" "$bad_folder/"
    if [ "$kind" = otherkey ]; then
        pkg repo "$bad_folder" rsa:/tmp/other.rsa >/dev/null
    else
        pkg repo "$bad_folder" >/dev/null
    fi
    write_repo_conf "$bad_conf/bad.conf" "splicefs$kind" "$bad_folder"
    pkg -o REPOS_DIR="$bad_conf" update -f -r "splicefs$kind" >/dev/null 2>&1 || true
    # pkg is a program here, so set -e is not off inside it.
    if pkg -o REPOS_DIR="$bad_conf" install -y -r "splicefs$kind" "$name" >/dev/null 2>&1; then
        fail "the package installed from a repository that is $kind"
    fi
done
echo "repository: signed catalog accepted; one signed with another key and an unsigned one refused"
phase_done "refuse the repositories with a wrong signature"

# The packages published before were needed for the catalog, and the host has them already:
# they do not travel back, so the result stays small however many versions are published.
for abiarch in amd64 aarch64; do
    old_packages="$input_dir/repo/FreeBSD:$major:$abiarch/All"
    [ -d "$old_packages" ] || continue
    for old in "$old_packages"/*.pkg; do
        [ -f "$old" ] || continue
        old_name="$(basename "$old")"
        [ "$old_name" = "$name-$version.pkg" ] || rm -f "$out/repo/FreeBSD:$major:$abiarch/All/$old_name"
    done
done
