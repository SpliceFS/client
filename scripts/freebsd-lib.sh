# shellcheck shell=sh
# Sourced by the scripts that need a FreeBSD machine (check-freebsd.sh, package-freebsd.sh).
# FreeBSD cannot run in a container, so each FreeBSD release in packaging/freebsd/images runs
# as a virtual machine under QEMU inside the Go container. The caller sets out (the absolute
# output folder), image (the container image QEMU is installed into) and defines fail.

# The machine has the CPU type of this computer: its own type runs fastest, and with KVM
# (Linux on amd64) at full speed.
host_cpu="$(uname -m)"
case "$host_cpu" in
    x86_64 | amd64) machine=amd64 ;;
    arm64 | aarch64) machine=arm64 ;;
    *) fail "no FreeBSD image for $host_cpu" ;;
esac

# A checksum tool that fails gives an empty answer, which the caller reports as a mismatch.
freebsd_checksum() {
    if command -v sha256sum >/dev/null 2>&1; then
        digest_line="$(sha256sum "$1")" || digest_line=""
    else
        digest_line="$(shasum -a 256 "$1")" || digest_line=""
    fi
    echo "${digest_line%% *}"
}

# Calls "$1 <release> <disk image>" for every release in the list that fits this machine,
# after fetching its image when it is not in the cache yet. FREEBSD limits the run to one
# release. An image is the official one of the FreeBSD project and must match the checksum
# in the list, so a changed download cannot slip into a build.
freebsd_each_release() {
    each="$1"
    # The caller sets out.
    # shellcheck disable=SC2154
    cache="$out/cache/freebsd"
    mkdir -p "$cache"
    ran=0
    while read -r release image_machine sum url; do
        # Blank lines and comments are skipped; any other line is a release.
        case "$release" in
            "" | "#"*) continue ;;
            *) ;;
        esac
        [ "$image_machine" = "$machine" ] || continue
        [ -z "${FREEBSD:-}" ] || [ "$FREEBSD" = "$release" ] || continue
        ran=$((ran + 1))
        packed="$cache/$(basename "$url")"
        disk="${packed%.xz}"
        if [ ! -f "$disk" ]; then
            echo "fetching FreeBSD $release ($machine)"
            curl -fsS -o "$packed.part" "$url"
            digest="$(freebsd_checksum "$packed.part")"
            [ "$digest" = "$sum" ] ||
                fail "$url does not have the checksum in packaging/freebsd/images"
            mv "$packed.part" "$packed"
            xz -d "$packed"
        fi
        # The loop reads the list on standard input, which the callback must not consume.
        "$each" "$release" "$disk" </dev/null
    done < packaging/freebsd/images
    [ "$ran" -gt 0 ] || fail "no image in packaging/freebsd/images for ${FREEBSD:-any release} on $machine"
}

# freebsd_job_folder <folder> <job script> [argument ...]
# Makes the folder the machine reads at its first boot: FreeBSD sees it as a small disk named
# cidata and runs the file user-data from it. The arguments are single words.
freebsd_job_folder() {
    folder="$1"
    script="$2"
    shift 2
    rm -rf "$folder"
    mkdir -p "$folder"
    cp "scripts/$script" "$folder/"
    sed -e "s|@JOB@|$script|" -e "s|@ARGS@|$*|" packaging/freebsd/user-data.in > "$folder/user-data"
    printf 'instance-id: %s\nlocal-hostname: build\n' "$(basename "$folder")" > "$folder/meta-data"
}

# freebsd_run <disk image> <job folder> <result folder> [input archive]
# Runs the machine to completion. Every path is under $out, which the container sees as /out.
# Sets run_status to the status of the run and never fails itself, so that the caller reports
# it (a function used as a condition runs with set -e off).
freebsd_run() {
    run_status=0
    source_dir="$(pwd)"
    kvm=""
    if [ "$machine" = amd64 ] && [ -e /dev/kvm ]; then kvm="--device /dev/kvm"; fi
    input=""
    if [ -n "${4:-}" ]; then input="/out/${4#"$out"/}"; fi
    # kvm and input are one word each or empty, so they are left unquoted on purpose. The
    # caller sets image and reads run_status.
    # shellcheck disable=SC2034,SC2086,SC2154
    docker run --rm $kvm -e VM_TIMEOUT="${VM_TIMEOUT:-900}" -v "$source_dir":/src:ro -v "$out":/out "$image" \
        sh /src/scripts/freebsd-vm.sh "$machine" "/out/${1#"$out"/}" "/out/${2#"$out"/}" \
        "/out/${3#"$out"/}" $input || run_status=$?
}
