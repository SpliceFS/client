#!/bin/sh
# Wraps the compiled program into Alpine packages, one per CPU type, with abuild. It runs
# inside an Alpine container (make package) after package-binaries.sh.
#
# abuild signs what it builds. The key made here lives only as long as this container, so
# these packages install with "apk add --allow-untrusted" until a publishing step signs
# them with the real key.
set -eu

out="${1:?usage: package-apk.sh OUTPUT_DIR}"
. scripts/package-lib.sh

apk add -q abuild >/dev/null
abuild-keygen -a -n -q

for arch in amd64 arm64; do
    case "$arch" in
        amd64) apkarch=x86_64 ;;
        arm64) apkarch=aarch64 ;;
    esac
    # abuild names the repository after the directory above the one holding the APKBUILD.
    work="$(mktemp -d)/splicefs/$name"
    mkdir -p "$work"
    cp packaging/alpine/"$name".* "$work/"
    sed "s/@VERSION@/$apkver/" packaging/alpine/APKBUILD > "$work/APKBUILD"
    (
        cd "$work"
        # CARCH labels the package for the CPU type of the binary inside it; nothing is
        # compiled here, so no cross compiler is involved.
        CARCH="$apkarch" STAGE="$out/stage/$arch" SRC="$OLDPWD" REPODEST="$work/repo" \
            abuild -F -q rootpkg
    )
    mkdir -p "$out/alpine/$apkarch"
    cp "$work/repo/splicefs/$apkarch/$name-$apkver-r0.apk" "$out/alpine/$apkarch/"
    echo "built alpine/$apkarch/$name-$apkver-r0.apk"
done
