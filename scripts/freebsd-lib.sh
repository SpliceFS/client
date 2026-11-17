# shellcheck shell=sh
# Sourced by the scripts that need a FreeBSD machine (check-freebsd.sh, package-freebsd.sh).
# FreeBSD cannot run in a container, so each FreeBSD release in packaging/freebsd/releases runs
# as a virtual machine under QEMU inside the prepared image of that release (ghcr.io/splicefs,
# named by FREEBSD_IMAGE in the Makefile). The disk in the image has been booted once and has
# pkg set up, so a run downloads, unpacks and installs nothing. The images are for amd64, and
# the machine needs hardware virtualization (KVM). The caller sets out (the absolute output
# folder) and defines fail.

# The device of hardware virtualization, on this computer and inside the container.
kvm_device="${KVM_DEVICE:-/dev/kvm}"

# Fails when the machine could only be emulated, which takes many times as long and looks like
# a hang. scripts/freebsd-require-kvm.sh runs it before anything is built.
freebsd_require_kvm() {
    [ -c "$kvm_device" ] ||
        fail "the FreeBSD machine needs hardware virtualization (KVM), and $kvm_device is not a device on this computer. Run on Linux on amd64 with KVM."
}

# The prepared image of a release: FREEBSD_IMAGE, which the caller sets, with the release put in.
# shellcheck disable=SC2154
freebsd_image() {
    echo "${FREEBSD_IMAGE%@RELEASE@*}$1${FREEBSD_IMAGE#*@RELEASE@}"
}

# Calls "$1 <release>" for every release in the list. FREEBSD limits the run to one release.
freebsd_each_release() {
    case "${FREEBSD_IMAGE:-}" in
        *@RELEASE@*) ;;
        *) fail "FREEBSD_IMAGE must name the image with @RELEASE@ where the release goes, for example ghcr.io/splicefs/freebsd-@RELEASE@:v2" ;;
    esac
    # A name of its own: shell variables are global, and a step that loops with a variable
    # named like this one would replace the step to call for the next release.
    freebsd_step="$1"
    ran=0
    while read -r release; do
        # Blank lines and comments are skipped; any other line is a release.
        case "$release" in
            "" | "#"*) continue ;;
            *) ;;
        esac
        [ -z "${FREEBSD:-}" ] || [ "$FREEBSD" = "$release" ] || continue
        ran=$((ran + 1))
        # The loop reads the list on standard input, which the callback must not consume.
        "$freebsd_step" "$release" </dev/null
    done < packaging/freebsd/releases
    [ "$ran" -gt 0 ] || fail "no release in packaging/freebsd/releases matches ${FREEBSD:-any release}"
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

# freebsd_run <release> <job folder> <result folder> [input archive]
# Runs the machine of the release to completion, with the machine runner that its image
# provides. Every path is under $out, which the container sees as /out.
# Sets run_status to the status of the run and never fails itself, so that the caller reports
# it (a function used as a condition runs with set -e off).
freebsd_run() {
    run_status=0
    source_dir="$(pwd)"
    vm_image="$(freebsd_image "$1")"
    input=""
    if [ -n "${4:-}" ]; then input="/out/${4#"$out"/}"; fi
    # input is one word or empty, so it is left unquoted on purpose. The caller sets out and
    # reads run_status.
    # shellcheck disable=SC2034,SC2086,SC2154
    docker run --rm --user 0 --device "$kvm_device" -e VM_TIMEOUT="${VM_TIMEOUT:-900}" \
        -v "$source_dir":/src:ro -v "$out":/out "$vm_image" \
        sh /src/scripts/freebsd-with-kvm.sh "$kvm_device" \
        /freebsd/vm.sh amd64 /freebsd/disk.qcow2 "/out/${2#"$out"/}" "/out/${3#"$out"/}" \
        $input || run_status=$?
}
