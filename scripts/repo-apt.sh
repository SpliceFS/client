#!/bin/sh
# Builds or updates the signed apt repository in OUTPUT_DIR/repo/apt from the .deb files in
# OUTPUT_DIR. It runs inside a Debian container (make repo), with the keys in /keys. The
# pool keeps every file already in it, so older versions stay installable.
set -eu

out="${1:?usage: repo-apt.sh OUTPUT_DIR}"
repo="$out/repo/apt"
suite=stable
archs="amd64 arm64"

mkdir -p "$repo/pool"
set -- "$out"/*.deb
[ -e "$1" ] || [ -n "$(find "$repo/pool" -name '*.deb' | head -n 1)" ] || {
    echo "FAIL: no .deb files in $out; run make package first" >&2
    exit 1
}

# apt-ftparchive is not in the build image. Quiet apt output, one line says what happened.
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq apt-utils >/dev/null

# A published version must never change, so a pool file with the same name and different
# content stops the run. Sorted input keeps the result the same on every run.
for deb in $(ls "$out"/*.deb 2>/dev/null | sort); do
    file="$(basename "$deb")"
    name="${file%%_*}"
    dest="$repo/pool/main/$(printf '%s' "$name" | cut -c1)/$name"
    if [ -f "$dest/$file" ]; then
        cmp -s "$deb" "$dest/$file" || {
            echo "FAIL: $file is already in the pool with different content" >&2
            exit 1
        }
        continue
    fi
    mkdir -p "$dest"
    cp "$deb" "$dest/$file"
done

# Paths in Packages are relative to the repository root, so the commands run from there.
cd "$repo"
rm -f "dists/$suite/Release" "dists/$suite/Release.gpg" "dists/$suite/InRelease"
for arch in $archs; do
    dir="dists/$suite/main/binary-$arch"
    mkdir -p "$dir"
    apt-ftparchive --arch "$arch" packages pool > "$dir/Packages"
    gzip -n -9 -c "$dir/Packages" > "$dir/Packages.gz"
done
apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=SpliceFS \
    -o APT::FTPArchive::Release::Label=SpliceFS \
    -o APT::FTPArchive::Release::Suite="$suite" \
    -o APT::FTPArchive::Release::Codename="$suite" \
    -o APT::FTPArchive::Release::Architectures="$archs" \
    -o APT::FTPArchive::Release::Components=main \
    release "dists/$suite" > "$out/Release.tmp"
mv "$out/Release.tmp" "dists/$suite/Release"

# The signing home lives in this container's temporary filesystem and dies with it. The
# secret file holds the signing subkey only, with no passphrase.
export GNUPGHOME="$(mktemp -d)"
chmod 700 "$GNUPGHOME"
gpg --batch --quiet --import /keys/splicefs-openpgp-signing-subkey.asc 2>/dev/null
fpr="$(gpg --list-secret-keys --with-colons | awk -F: '$1=="fpr"{print $10; exit}')"
gpg --batch --yes --pinentry-mode loopback --passphrase "" --default-key "$fpr" \
    --armor --detach-sign -o "dists/$suite/Release.gpg" "dists/$suite/Release"
gpg --batch --yes --pinentry-mode loopback --passphrase "" --default-key "$fpr" \
    --clearsign -o "dists/$suite/InRelease" "dists/$suite/Release"

echo "built apt repository: $(find pool -name '*.deb' | wc -l | tr -d ' ') packages"
