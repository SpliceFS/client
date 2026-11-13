#!/bin/sh
# Builds or updates the signed Alpine repositories in OUTPUT_DIR/repo/alpine, one per CPU
# type, from the packages in OUTPUT_DIR/alpine. It runs inside an Alpine container (make
# repo), with the keys in /keys. apk checks every package file and the index against a key
# in /etc/apk/keys, found by the key's file name, so the index is signed as splicefs.rsa.pub
# and the packages were built with the same key. Files already in the repository stay, so
# older versions stay installable.
set -eu

out="${1:?usage: repo-apk.sh OUTPUT_DIR}"
repo="$out/repo/alpine"

apk add -q abuild >/dev/null

# The private key is copied into this container's temporary filesystem, where abuild-sign
# can read it whatever the mode on the host is. The public name comes from the file name.
keydir="$(mktemp -d)"
cp /keys/splicefs.rsa /keys/splicefs.rsa.pub "$keydir/"
chmod 600 "$keydir/splicefs.rsa"
# apk index checks each package signature, so the index step also proves the packages were
# built with the repository key.
cp /keys/splicefs.rsa.pub /etc/apk/keys/splicefs.rsa.pub

# A published version must never change, so a file with the same name and different content
# stops the run. That includes a rebuild of the same version, which signs the package again.
changed=""
for apk in $(ls "$out"/alpine/*/*.apk 2>/dev/null | sort); do
    file="$(basename "$apk")"
    arch="$(basename "$(dirname "$apk")")"
    dest="$repo/$arch"
    mkdir -p "$dest"
    if [ -f "$dest/$file" ]; then
        cmp -s "$apk" "$dest/$file" || {
            echo "FAIL: $file is already in the repository with different content" >&2
            exit 1
        }
        continue
    fi
    cp "$apk" "$dest/$file"
    changed="$changed $arch"
done

for dir in "$repo"/*/; do
    arch="$(basename "$dir")"
    # Nothing new for this CPU type and an index exists: leave the signed index as it is.
    case " $changed " in
        *" $arch "*) ;;
        *) [ ! -f "$dir/APKINDEX.tar.gz" ] || continue ;;
    esac
    # --rewrite-arch makes the index describe this CPU type whatever CPU built the index.
    (
        cd "$dir"
        rm -f APKINDEX.tar.gz
        apk index -q --rewrite-arch "$arch" -o APKINDEX.tar.gz ./*.apk
        abuild-sign -q -k "$keydir/splicefs.rsa" -p "$keydir/splicefs.rsa.pub" APKINDEX.tar.gz
    )
    count="$(find "$dir" -maxdepth 1 -name '*.apk' | wc -l | tr -d ' ')"
    echo "built alpine repository $arch: $count packages"
done
