#!/bin/sh
# Decides which keys every repository step signs with and puts them in dist/keys/. Runs on
# the host (make repo). Environment: SIGNING_DIR names a folder with the real keys
# (splicefs-openpgp-signing-subkey.asc, splicefs.rsa and the public halves splicefs.asc and
# splicefs.rsa.pub); without it, throwaway test keys are generated once and reused. Later
# steps read only dist/keys, so this is the one place that chooses. Private keys are never
# printed, and only ever mounted read-only into a container.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
keys="$root/dist/keys"
files="splicefs-openpgp-signing-subkey.asc splicefs.asc splicefs.rsa splicefs.rsa.pub"
image="${LINUX_IMAGE:-golang:1.27}"

# Sets missing to the names in $files that the folder $1 lacks, empty when it has them all.
# A function that is used as a condition runs with set -e off, so it reports in a variable.
find_missing() {
    missing=""
    for f in $files; do
        [ -f "$1/$f" ] || missing="$missing $f"
    done
}

mkdir -p "$keys"
find_missing "$keys"
keys_missing="$missing"

if [ -n "${SIGNING_DIR:-}" ]; then
    find_missing "$SIGNING_DIR"
    [ -z "$missing" ] || {
        echo "FAIL: $SIGNING_DIR must hold: $files" >&2
        exit 1
    }
    for f in $files; do
        cp "$SIGNING_DIR/$f" "$keys/$f"
    done
    echo "signing keys: real keys from SIGNING_DIR"
elif [ -z "$keys_missing" ]; then
    echo "signing keys: throwaway test keys in dist/keys (never publish packages signed with them)"
else
    # The container has gpg and openssl, so the host needs neither. The OpenPGP home lives in
    # the container's own temporary filesystem. The primary key only certifies; the secret
    # export holds just the signing subkey, the way a real signing key is kept.
    host_uid="$(id -u)"
    host_gid="$(id -g)"
    docker run --rm --user "$host_uid:$host_gid" -e HOME=/tmp -v "$keys":/keys "$image" sh -eu -c '
        export GNUPGHOME="$(mktemp -d)"
        chmod 700 "$GNUPGHOME"
        uid="SpliceFS test key <test@example.invalid>"
        gpg --batch --pinentry-mode loopback --passphrase "" \
            --quick-generate-key "$uid" rsa4096 cert never 2>/dev/null
        fpr="$(gpg --list-keys --with-colons | awk -F: "\$1==\"fpr\"{print \$10; exit}")"
        gpg --batch --pinentry-mode loopback --passphrase "" \
            --quick-add-key "$fpr" rsa4096 sign never 2>/dev/null
        gpg --batch --pinentry-mode loopback --passphrase "" --armor \
            --export-secret-subkeys "$fpr" > /keys/splicefs-openpgp-signing-subkey.asc
        gpg --armor --export "$fpr" > /keys/splicefs.asc
        openssl genrsa -out /keys/splicefs.rsa 4096 2>/dev/null
        openssl rsa -in /keys/splicefs.rsa -pubout -out /keys/splicefs.rsa.pub 2>/dev/null
    '
    echo "signing keys: generated THROWAWAY test keys in dist/keys; packages signed with them"
    echo "must never be published"
fi

chmod 600 "$keys/splicefs-openpgp-signing-subkey.asc" "$keys/splicefs.rsa"
chmod 644 "$keys/splicefs.asc" "$keys/splicefs.rsa.pub"
