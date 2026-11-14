#!/bin/sh
# Publishes the package repositories in dist/repo to packages.splicefs.org by rsync, or
# proves that the upload path works. Runs on the host (make publish-fetch, publish,
# publish-check, and the release workflow).
# Usage: publish.sh fetch | publish.sh send | publish.sh check
#   fetch  downloads what is published now into dist/repo, so the repository that is built
#          keeps every older version. An empty remote is fine (the first release).
#   send   uploads dist/repo in two passes, the packages first and the index files after them;
#          a published package is never deleted.
#   check  lists the remote root and changes nothing.
# Variables: REPO_DIR (default dist/repo), UPLOAD_HOST (packages.splicefs.org), UPLOAD_PORT
# (7022), UPLOAD_USER (site-splicefs-packages), UPLOAD_KEY_FILE (required, private key,
# mode 600), UPLOAD_KNOWN_HOSTS_FILE (required). The server forces a restricted rsync whose
# root is this repository's folder, so the remote path is always /. Test hook: UPLOAD_TARGET
# is a complete rsync destination, for example a local folder. It replaces user@host:/ and no
# ssh options are used then, so the checks can run without a server.
set -eu

MODE=${1:-}
case "$MODE" in
  fetch | send | check) ;;
  *) echo "usage: publish.sh fetch|send|check" >&2; exit 2 ;;
esac

ROOT=$(cd "$(dirname "$0")/.." && pwd)
REPO=${REPO_DIR:-$ROOT/dist/repo}
HOST=${UPLOAD_HOST:-packages.splicefs.org}
PORT=${UPLOAD_PORT:-7022}
USER_NAME=${UPLOAD_USER:-site-splicefs-packages}

if [ -n "${UPLOAD_TARGET:-}" ]; then
  # The trailing slash makes rsync copy the content of the folder, as it does for remote /.
  DEST=${UPLOAD_TARGET%/}/
else
  : "${UPLOAD_KEY_FILE:?UPLOAD_KEY_FILE is not set}"
  : "${UPLOAD_KNOWN_HOSTS_FILE:?UPLOAD_KNOWN_HOSTS_FILE is not set}"
  [ -f "$UPLOAD_KEY_FILE" ] || { echo "UPLOAD_KEY_FILE is not a file" >&2; exit 1; }
  [ -f "$UPLOAD_KNOWN_HOSTS_FILE" ] || { echo "UPLOAD_KNOWN_HOSTS_FILE is not a file" >&2; exit 1; }
  # ssh refuses a loose key anyway; failing here gives a clearer message before any connect.
  # shellcheck disable=SC2012 # ls prints the same mode string on macOS and Linux, stat does not
  KEY_MODE=$(ls -ld "$UPLOAD_KEY_FILE" | cut -c1-10)
  if [ "$KEY_MODE" != '-rw-------' ]; then
    echo "UPLOAD_KEY_FILE must have mode 600" >&2
    exit 1
  fi
  # Only the given key and the pinned host key are trusted: no agent, no prompts, no
  # fallback to the user's own ssh configuration.
  SSH_CMD="ssh -p $PORT -i $UPLOAD_KEY_FILE -o IdentitiesOnly=yes -o IdentityAgent=none"
  SSH_CMD="$SSH_CMD -o BatchMode=yes -o StrictHostKeyChecking=yes"
  SSH_CMD="$SSH_CMD -o UserKnownHostsFile=$UPLOAD_KNOWN_HOSTS_FILE -F /dev/null"
  DEST=$USER_NAME@$HOST:/
fi

# Runs rsync with the common options: -e only when there is a real host.
run_rsync() {
  if [ -n "${UPLOAD_TARGET:-}" ]; then
    rsync "$@"
  else
    rsync -e "$SSH_CMD" "$@"
  fi
}

if [ "$MODE" = check ]; then
  # Read only: lists the remote root and prints the entry names.
  run_rsync --list-only "$DEST" |
    awk '{ $1 = $2 = $3 = $4 = ""; sub(/^ +/, ""); if ($0 != "" && $0 != ".") print }'
  exit 0
fi

if [ "$MODE" = fetch ]; then
  # No delete option: a half built local repository is never emptied by a fetch, and the
  # repository scripts add to what is there.
  mkdir -p "$REPO"
  run_rsync -a "$DEST" "$REPO/"
  echo "fetched ${UPLOAD_TARGET:-$HOST} into $REPO"
  exit 0
fi

# An unsigned or partial repository must never go live, so every file has to exist first.
for f in apt/dists/stable/InRelease rpm/x86_64/repodata/repomd.xml.asc \
  rpm/aarch64/repodata/repomd.xml.asc alpine/x86_64/APKINDEX.tar.gz \
  alpine/aarch64/APKINDEX.tar.gz; do
  [ -f "$REPO/$f" ] || { echo "missing $REPO/$f, run make repo first" >&2; exit 1; }
done
# The FreeBSD versions come from the list the packages are built for, so they are written
# down once. Each major version has one folder per CPU type.
majors=$(awk '!/^#/ && NF { split($1, v, "."); print v[1] }' \
  "$ROOT/packaging/freebsd/images" | sort -un)
for major in $majors; do
  for arch in amd64 aarch64; do
    f=freebsd/FreeBSD:$major:$arch/meta.conf
    [ -f "$REPO/$f" ] || { echo "missing $REPO/$f, run make package-freebsd first" >&2; exit 1; }
  done
done

# A published package must never disappear, and the repository built from fetch holds every
# old package, so a deletion means something went wrong. Only two kinds of file may go:
# everything inside a repodata folder (its files carry a hash in the name, so every new build
# replaces them) and the index files of a format (APKINDEX.tar.gz, Packages*, Release*,
# InRelease), which a new build rewrites.
OWN_ALLOWED='(^|/)repodata/'
OWN_ALLOWED="$OWN_ALLOWED"'|(^|/)(APKINDEX\.tar\.gz|InRelease|Packages(\.[a-z0-9]+)*'
OWN_ALLOWED="$OWN_ALLOWED"'|Release(\.[a-z0-9]+)*)$'
DRY=$(mktemp)
trap 'rm -f "$DRY"' EXIT
trap 'exit 1' HUP INT TERM
# set -e is switched off around the call, not by using it as a condition, which would also
# switch it off inside run_rsync.
set +e
run_rsync -a --chmod=D755,F644 --delete --dry-run --itemize-changes "$REPO/" "$DEST" \
  > "$DRY"
dry_status=$?
set -e
if [ "$dry_status" -ne 0 ]; then
  echo "the dry run failed, nothing was sent" >&2
  exit 1
fi
# A deleted folder is listed with a trailing slash after its files, each of which was checked.
BAD=$(awk '$1 == "*deleting" { $1 = ""; sub(/^ +/, ""); print }' "$DRY" |
  grep -v '/$' | grep -Ev "$OWN_ALLOWED" || true)
if [ -n "$BAD" ]; then
  echo "refusing to send: it would delete published files:" >&2
  echo "$BAD" | sed 's/^/  /' >&2
  exit 1
fi

# The index files are what apt, dnf, apk and pkg read to learn which packages exist: the apt
# dists folder, the rpm repodata folders, APKINDEX.tar.gz, the FreeBSD catalog (meta,
# meta.conf, packagesite.pkg, data.pkg) and the signature files next to them. A new index
# that is in place before the packages it names are makes a client ask for a file that is not
# there yet. rsync --delay-updates cannot prevent that alone, because it moves the files into
# place one after another. So the upload runs in two passes and the script stops at the first
# one that fails: pass 1 sends everything except the index files, pass 2 sends the index
# files (and does the deletions). The patterns start with / so that they match from the top of
# the repository folder only.
INDEX_PATTERNS='/apt/dists/*** /rpm/*/repodata/*** /alpine/*/APKINDEX.tar.gz'
INDEX_PATTERNS="$INDEX_PATTERNS /alpine/*/*.sig /alpine/*/*.asc"
INDEX_PATTERNS="$INDEX_PATTERNS /freebsd/*/meta /freebsd/*/meta.conf"
INDEX_PATTERNS="$INDEX_PATTERNS /freebsd/*/packagesite.pkg /freebsd/*/data.pkg"
INDEX_PATTERNS="$INDEX_PATTERNS /freebsd/*/*.sig /freebsd/*/*.asc"

# Runs one pass: $1 names it for the message, the other arguments are rsync options. A failed
# pass ends the script, so a later pass never sends an index for files that an earlier one did
# not deliver. set -e is switched off around the call, not by using it as a condition, which
# would also switch it off inside run_rsync.
send_pass() {
  pass_name=$1
  shift
  set +e
  run_rsync "$@" "$REPO/" "$DEST"
  pass_status=$?
  set -e
  if [ "$pass_status" -ne 0 ]; then
    echo "the upload stopped: $pass_name failed" >&2
    exit 1
  fi
}

# Pass 1: no deletions, and every index file left out. The patterns hold * and must reach
# rsync as they are, so file name expansion is off while the options are built.
set -f
set -- -a --chmod=D755,F644 --delay-updates
for pattern in $INDEX_PATTERNS; do
  set -- "$@" "--exclude=$pattern"
done
set +f
send_pass "pass 1 (everything except the index files)" "$@"

# Pass 2: the same upload again, now with the deletions. Everything pass 1 sent is there and
# unchanged, so rsync skips it, and what is left to send is the index files. --delete-after
# removes only after every file has arrived. The filter rules are checked in order and the
# first match wins: R lets rsync delete what matches (repodata folders and the index files,
# the same set the dry run allowed), and the final P * protects everything else, so even a
# file the dry run missed cannot be deleted.
set -- -a --chmod=D755,F644 --delay-updates --delete-after \
  --filter='R repodata/***' --filter='R APKINDEX.tar.gz' --filter='R Packages*' \
  --filter='R Release*' --filter='R InRelease' --filter='P *'
send_pass "pass 2 (the index files and the deletions)" "$@"
echo "published to ${UPLOAD_TARGET:-$HOST}"
