#!/bin/sh
# Builds or updates the signed rpm repositories in OUTPUT_DIR/repo/rpm, one per CPU type, from
# the .rpm files in OUTPUT_DIR. It runs inside a Rocky Linux container (make repo), with the
# keys in /keys. Every rpm is signed, and so is the index (repomd.xml), because dnf checks
# the packages with gpgcheck and the index with repo_gpgcheck. Rpms already in the
# repository stay, so older versions stay installable.
set -eu

out="${1:?usage: repo-rpm.sh OUTPUT_DIR}"
repo="$out/repo/rpm"

set -- "$out"/*.rpm
[ -e "$1" ] || [ -n "$(find "$repo" -name '*.rpm' 2>/dev/null | head -n 1)" ] || {
    echo "FAIL: no .rpm files in $out; run make package first" >&2
    exit 1
}

dnf install -y -q createrepo_c rpm-sign gnupg2 >/dev/null

# The signing home lives in this container's temporary filesystem and dies with it. The
# secret file holds the signing subkey only, with no passphrase. The rpm macros are given on
# the command line, so nothing about signing is written into the repository or the image.
export GNUPGHOME="$(mktemp -d)"
chmod 700 "$GNUPGHOME"
gpg --batch --quiet --import /keys/splicefs-openpgp-signing-subkey.asc 2>/dev/null
fpr="$(gpg --list-secret-keys --with-colons | awk -F: '$1=="fpr"{print $10; exit}')"
sign_define="--define '_gpg_name $fpr' --define '_gpg_path $GNUPGHOME'"

# Sorted input keeps the result the same on every run.
changed=""
for rpm in $(ls "$out"/*.rpm 2>/dev/null | sort); do
    file="$(basename "$rpm")"
    arch="${file%.rpm}"
    arch="${arch##*.}"
    dest="$repo/$arch"
    mkdir -p "$dest"
    if [ -f "$dest/$file" ]; then
        # Signing changes the file, so the payload digest decides: a published version must
        # never change, and the payload is the part signing leaves alone.
        new="$(rpm -qp --qf '%{PAYLOADDIGEST}' "$rpm" 2>/dev/null)"
        old="$(rpm -qp --qf '%{PAYLOADDIGEST}' "$dest/$file" 2>/dev/null)"
        [ -n "$new" ] && [ "$new" = "$old" ] || {
            echo "FAIL: $file is already in the repository with different content" >&2
            exit 1
        }
        continue
    fi
    cp "$rpm" "$dest/$file"
    # The copy is signed, never the file in OUTPUT_DIR. stdin is closed so a missing
    # passphrase fails instead of waiting for a terminal.
    eval rpmsign --addsign "$sign_define" "$dest/$file" </dev/null >/dev/null
    changed="$changed $arch"
done

for dir in "$repo"/*/; do
    arch="$(basename "$dir")"
    meta="$dir/repodata"
    # Nothing new for this CPU type and an index exists: leave the signed index as it is.
    case " $changed " in
        *" $arch "*) ;;
        *) [ ! -f "$meta/repomd.xml.asc" ] || continue ;;
    esac
    createrepo_c --quiet --update "$dir"
    rm -f "$meta/repomd.xml.asc" "$meta/repomd.xml.key"
    gpg --batch --yes --pinentry-mode loopback --passphrase "" --default-key "$fpr" \
        --armor --detach-sign -o "$meta/repomd.xml.asc" "$meta/repomd.xml"
    # zypper looks for the public key next to the index.
    cp /keys/splicefs.asc "$meta/repomd.xml.key"
    count="$(find "$dir" -maxdepth 1 -name '*.rpm' | wc -l | tr -d ' ')"
    echo "built rpm repository $arch: $count packages"
done
