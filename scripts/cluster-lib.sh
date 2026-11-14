# shellcheck shell=sh
# Sourced by cluster-test.sh, which sets work (the temporary folder), server_bin, client_bin
# and base_port before it sources this file. The scenario files in cluster-scenarios/ call the
# helpers below and read share, time_old, time_mid, time_new and picked_hash, which ShellCheck
# cannot see from this file alone, so both warnings are off here.
# shellcheck disable=SC2034,SC2154
#
# How a scenario file looks: it defines one function per scenario and registers each with
#
#   scenario "name that is printed and matched by CLUSTER_ONLY" function_name
#
# The runner calls begin_scenario before the function and end_scenario after it, so the
# function never does. Paths given to the expect_* checks and to slow_read_start are relative
# to the share folder of the mount ("." is the share itself); a path that starts with / is
# used as it is.

# The PID of the running client, and of a program that wait_until must not outlive (when it
# has ended, waiting longer for it only delays the report).
client_pid=""
watch_pid=""
# The background reader of slow_read_start.
slow_pid=""
# The name of the running scenario, for the failure message.
current_scenario="set-up"
# With a mount path of "/" every share of the cluster is a folder below the mount point
# (client docs, "Mounts"), so the share media is this folder.
share="$work/mnt/media"
# Fixed modification times for the scenarios that need one copy to be newer than another.
time_old="2026-01-01 12:00:00"
time_mid="2026-03-01 12:00:00"
time_new="2026-06-01 12:00:00"
# The registered scenarios, one per line: the function name, a space, the scenario name.
scenario_list="$work/scenarios.list"
: >"$scenario_list"

# The state letter of a process, or nothing when it is gone. A program that ended and was
# not waited for yet is a zombie (Z) and has ended too; kill -0 cannot tell the two apart.
process_state() {
    awk '/^State:/ { print $2 }' "/proc/$1/status" 2>/dev/null || true
}

process_ended() {
    ended_state="$(process_state "$1")"
    case "$ended_state" in
        "" | Z) return 0 ;;
        *) return 1 ;;
    esac
}

# Asks a program to end, kills it after 20 s and collects it.
stop_process() {
    [ -n "$1" ] || return 0
    kill "$1" 2>/dev/null || true
    stopped=0
    while :; do
        stop_state="$(process_state "$1")"
        case "$stop_state" in
            "" | Z) break ;;
            *) ;;
        esac
        if [ "$stopped" -ge 20 ]; then
            kill -9 "$1" 2>/dev/null || true
            break
        fi
        sleep 1
        stopped=$((stopped + 1))
    done
    wait "$1" 2>/dev/null || true
}

show_logs() {
    for log_name in check slow; do
        if [ -s "$work/$log_name.txt" ]; then
            echo "--- the last output of $log_name.txt ---"
            cat "$work/$log_name.txt"
        fi
    done
    for log_name in client server1 server2 server3 server4 server5; do
        if [ -f "$work/$log_name.log" ]; then
            echo "--- last 30 lines of the $log_name log ---"
            tail -n 30 "$work/$log_name.log"
        fi
    done
}

# Runs on every way out. The client is asked first so that it unmounts itself; the lazy
# unmount is for a client that could not. The folder is removed only when nothing is
# mounted in it, so that a mount that is still there can never lead into the data.
cleanup() {
    status=$?
    trap - EXIT INT TERM
    if [ "$status" -ne 0 ]; then show_logs >&2; fi
    stop_process "$slow_pid"
    stop_process "$client_pid"
    umount -l "$work/mnt" 2>/dev/null || true
    for cleanup_number in 1 2 3 4 5; do
        if [ -f "$work/server$cleanup_number.pid" ]; then
            cleanup_pid="$(cat "$work/server$cleanup_number.pid")"
            stop_process "$cleanup_pid"
        fi
    done
    if mountpoint -q "$work/mnt"; then
        echo "cluster-test.sh: $work/mnt is still mounted, folder kept" >&2
    else
        rm -rf "$work"
    fi
    exit "$status"
}

# A failed check names its scenario; cleanup prints the logs.
fail() {
    echo "cluster-test.sh: scenario \"$current_scenario\": $*" >&2
    exit 1
}

# wait_until SECONDS WHAT COMMAND...: runs the command once a second until it succeeds.
wait_until() {
    limit="$1"
    what="$2"
    shift 2
    waited=0
    until "$@"; do
        if [ -n "$watch_pid" ]; then
            watch_state="$(process_state "$watch_pid")"
            case "$watch_state" in
                "" | Z) fail "a program ended while waiting for: $what" ;;
                *) ;;
            esac
        fi
        waited=$((waited + 1))
        [ "$waited" -le "$limit" ] || fail "after $limit s still waiting for: $what"
        sleep 1
    done
}

# The port of server N.
port_of() {
    echo $((base_port + $1 - 1))
}

# Sets resolved_path to the path of REL below the share of the mount; a path that starts
# with / is already complete. It sets a variable and prints nothing, so that the checks need
# no command substitution.
resolve_path() {
    case "$1" in
        /*) resolved_path="$1" ;;
        *) resolved_path="$share/$1" ;;
    esac
}

# Sorted list of the files below a folder, as paths relative to it.
list_files() {
    (cd "$1" && find . -type f | LC_ALL=C sort)
}

# The SHA-256 of a file, without the file name. A file that cannot be read fails the check
# here (the exit of fail ends only the substitution this runs in, and set -e in the caller
# ends the script with the same status).
hash_of() {
    sum_line="$(sha256sum <"$1")" || fail "cannot read: $1"
    printf '%s\n' "${sum_line%% *}"
}

# The users, the signing keys and the node list that all servers share. The password hash
# comes from the server program, so it is written here once and not per server.
make_shared() {
    mkdir -p "$work/shared/users"
    secret_hash="$(printf 'secret\n' | "$server_bin" hash-password)"
    [ -n "$secret_hash" ] || fail "hash-password printed nothing"
    cat >"$work/shared/users/alice.yaml" <<EOF
password_hash: "$secret_hash"
shares: [media]
EOF
}

# The files every scenario that needs "the same five files" copies. They are made once, so
# the 12 MiB random file costs one read of /dev/urandom for the whole run.
make_source() {
    mkdir -p "$work/source"
    put_text "$work/source" hello.txt 'hello from the cluster'
    : >"$work/source/empty.txt"
    put_text "$work/source" "with space.txt" 'the name has a space'
    put_text "$work/source" one/two/deep.txt 'two folders deep'
    # More than two blocks of 5 MiB, so a read spans several requests.
    put_random "$work/source" big.bin $((12 * 1024 * 1024))
}

# The file that only the first scenario copies. A block is 5 MiB and the client keeps 4
# requests in flight per server, so a 12 MiB file is read from one server alone. 96 MiB is 20
# blocks, enough for every server to be asked for some. It is made once, apart from the five
# files, so the other scenarios do not carry it.
make_large_source() {
    mkdir -p "$work/source-large"
    put_random "$work/source-large" large.bin $((96 * 1024 * 1024))
}

# put_text FOLDER NAME TEXT and put_random FOLDER NAME BYTES write one file, with its
# folders. A scenario in which two servers hold one name with different contents calls
# them once per server.
put_text() {
    put_target="$1/$2"
    mkdir -p "${put_target%/*}"
    printf '%s\n' "$3" >"$put_target"
}

put_random() {
    put_target="$1/$2"
    mkdir -p "${put_target%/*}"
    head -c "$3" /dev/urandom >"$put_target"
}

# put_copy FROM_FILE N REL_NAME copies a file into the data folder of server N. cp -p keeps
# size and modification time, so every server that gets a copy holds the same version.
put_copy() {
    copy_target="$work/data$2/$3"
    mkdir -p "${copy_target%/*}"
    cp -p "$1" "$copy_target"
}

put_empty_dir() {
    mkdir -p "$work/data$1/$2"
}

# A file or a folder, whichever REL_NAME is.
remove_from() {
    rm -rf "${work:?}/data$1/$2"
}

# Copies the five files into the data folder of server N.
copy_source() {
    cp -pR "$work/source/." "$work/data$1/"
}

copy_large() {
    cp -p "$work/source-large/large.bin" "$work/data$1/"
}

# Sets the modification time of a file. GNU touch; the script runs on Linux only.
stamp() {
    touch -d "$3" "$1/$2"
}

# The folders and the configuration of server N, apart from its share: its own data folder
# (the share media), its own state folder, and "shared" in it as a link to the one shared
# folder. The link is made before the first start. Every server has its own node name,
# because of two servers with one name the node list keeps only one.
make_server_base() {
    make_port="$(port_of "$1")"
    mkdir -p "$work/data$1" "$work/etc$1/conf.d" "$work/etc$1/shares.d" "$work/state$1"
    ln -sfn "$work/shared" "$work/state$1/shared"
    cat >"$work/etc$1/conf.d/10-server.yaml" <<EOF
listen:
  - address: 127.0.0.1
    port: $make_port
    plain: true
cluster:
  node: n$1
  url: "http://127.0.0.1:$make_port/api"
log:
  level: info
  format: text
EOF
}

make_server() {
    make_server_base "$1"
    printf 'path: %s\n' "$work/data$1" >"$work/etc$1/shares.d/media.yaml"
}

# A server that runs but offers no share: shares.d stays empty.
make_server_without_share() {
    make_server_base "$1"
}

server_answers() {
    answer_port="$(port_of "$1")"
    curl -fsS --max-time 2 "http://127.0.0.1:$answer_port/api/v1/health" >/dev/null 2>&1
}

# Starts server N and waits for its health answer. The process number goes into a file,
# because a shell has no list of numbered variables. A scenario that made the server itself
# (make_server_without_share) keeps that configuration.
start_server() {
    if [ ! -f "$work/etc$1/conf.d/10-server.yaml" ]; then make_server "$1"; fi
    "$server_bin" --config "$work/etc$1" --state "$work/state$1" >"$work/server$1.log" 2>&1 &
    started_pid=$!
    printf '%s\n' "$started_pid" >"$work/server$1.pid"
    watch_pid="$started_pid"
    wait_until 20 "server $1 answers on its health URL" server_answers "$1"
    watch_pid=""
}

stop_server() {
    [ -f "$work/server$1.pid" ] || return 0
    server_pid="$(cat "$work/server$1.pid")"
    stop_process "$server_pid"
    rm -f "$work/server$1.pid"
}

# Puts node N in the node list without running it: the list is every file in nodes/, and a
# server that is stopped stays in it as well, so a file for a port where nothing listens
# is the same thing. The name is 16 hex digits.
write_idle_node() {
    idle_port="$(port_of "$1")"
    idle_file="$(printf 'ffffffffffffff%02x' "$1")"
    mkdir -p "$work/shared/nodes"
    printf 'name: n%s\nurl: http://127.0.0.1:%s/api\n' "$1" "$idle_port" \
        >"$work/shared/nodes/$idle_file.yaml"
}

# The client settings of one scenario. An empty value means the client's own default, which
# is what every scenario gets unless it asks for something else.
reset_client_settings() {
    setting_listing_ttl=""
    setting_revalidate=""
    setting_block_size=""
    setting_streams_per_node=""
    setting_read_ahead=""
}
reset_client_settings

# client_setting KEY VALUE, before the client starts. listing_ttl and revalidate go into the
# cache section of conf.d, the others into the cluster file.
client_setting() {
    case "$1" in
        listing_ttl) setting_listing_ttl="$2" ;;
        revalidate) setting_revalidate="$2" ;;
        block_size) setting_block_size="$2" ;;
        streams_per_node) setting_streams_per_node="$2" ;;
        read_ahead) setting_read_ahead="$2" ;;
        *) fail "client_setting: unknown setting $1" ;;
    esac
}

# The client's configuration. Its cluster host is server 1; the other servers it learns from
# the node list.
make_client_config() {
    first_port="$(port_of 1)"
    mkdir -p "$work/c/conf.d" "$work/c/clusters.d" "$work/c/mounts.d" "$work/mnt"
    {
        printf 'cache:\n  dir: %s\n' "$work/cache"
        if [ -n "$setting_listing_ttl" ]; then printf '  listing_ttl: %s\n' "$setting_listing_ttl"; fi
        if [ -n "$setting_revalidate" ]; then printf '  revalidate: %s\n' "$setting_revalidate"; fi
    } >"$work/c/conf.d/10-cache.yaml"
    {
        printf 'url: http://127.0.0.1:%s/api\nuser: alice\npassword_file: secret\n' "$first_port"
        if [ -n "$setting_block_size" ]; then printf 'block_size: %s\n' "$setting_block_size"; fi
        if [ -n "$setting_streams_per_node" ]; then
            printf 'streams_per_node: %s\n' "$setting_streams_per_node"
        fi
        if [ -n "$setting_read_ahead" ]; then printf 'read_ahead: %s\n' "$setting_read_ahead"; fi
    } >"$work/c/clusters.d/test.yaml"
    printf 'secret\n' >"$work/c/secret"
    chmod 600 "$work/c/secret"
    printf 'cluster: test\ntarget: %s\n' "$work/mnt" >"$work/c/mounts.d/m.yaml"
}

share_listable() {
    mountpoint -q "$work/mnt" && ls "$share" >/dev/null 2>&1
}

start_client() {
    make_client_config
    "$client_bin" --config "$work/c" mount >"$work/client.log" 2>&1 &
    client_pid=$!
    watch_pid="$client_pid"
    wait_until 30 "the mount at $work/mnt can be listed" share_listable
    watch_pid=""
}

not_mounted() {
    ! mountpoint -q "$work/mnt"
}

stop_client() {
    [ -n "$client_pid" ] || return 0
    kill -TERM "$client_pid" 2>/dev/null || true
    wait_until 20 "the client ends after SIGTERM" process_ended "$client_pid"
    wait "$client_pid" || true
    client_pid=""
    wait_until 20 "$work/mnt is no mount point" not_mounted
}

restart_client() {
    stop_client
    start_client
}

restart_client_empty_cache() {
    stop_client
    empty_cache
    start_client
}

# The folder goes, not only what is in it, so the client makes it as it does on a first
# start.
empty_cache() {
    rm -rf "$work/cache"
}

clear_data() {
    for clear_number in 1 2 3 4 5; do
        rm -rf "$work/data$clear_number"
        mkdir -p "$work/data$clear_number"
    done
}

# Every scenario starts here: nothing runs, no data, no cache, an empty node list, default
# client settings, and no state or configuration that one server keeps for itself (so a
# server made without a share in one scenario has one in the next).
begin_scenario() {
    current_scenario="$1"
    stop_client
    for begin_number in 1 2 3 4 5; do
        stop_server "$begin_number"
        rm -rf "$work/state$begin_number/local" "${work:?}/etc$begin_number"
    done
    clear_data
    empty_cache
    rm -rf "$work/shared/nodes"
    mkdir -p "$work/shared/nodes"
    reset_client_settings
    : >"$work/check.txt"
    : >"$work/slow.txt"
    slow_pid=""
}

end_scenario() {
    echo "ok: $current_scenario"
}

# A check that follows a start may run before the client has heard from every server, so each
# check compares again once a second for at most 15 s (or the seconds it is given as its
# last argument) and fails only when it still differs. What the last comparison found is left
# in check.txt for the failure report.
trees_equal() {
    list_files "$1" >"$work/files.want"
    list_files "$share" >"$work/files.got" 2>/dev/null
    diff -u "$work/files.want" "$work/files.got" >"$work/check.txt" 2>&1
}

# The files below the mount are the files below DIR (names only).
expect_same_tree() {
    wait_until "${2:-15}" "the files below the mount are the files of $1" trees_equal "$1"
}

hash_is() {
    if [ ! -f "$1" ]; then
        printf 'missing: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    is_sum="$(sha256sum <"$1" 2>"$work/check.txt")" || return 1
    is_hash="${is_sum%% *}"
    if [ "$is_hash" != "$2" ]; then
        printf '%s\nhas SHA-256 %s\nwanted      %s\n' "$1" "$is_hash" "$2" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# expect_hash REL SHA [SECONDS]: the file has this SHA-256 (as hash_of prints it).
expect_hash() {
    resolve_path "$1"
    wait_until "${3:-15}" "$1 has the wanted content" hash_is "$resolved_path" "$2"
}

# The file has the SHA-256 HASH1 or HASH2, and never anything else, which would be a mix of
# the two copies. Which one it was is left in picked_hash.
hash_is_either() {
    either_hash=""
    if [ ! -f "$1" ]; then
        printf 'missing: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    either_sum="$(sha256sum <"$1" 2>"$work/check.txt")" || return 1
    either_hash="${either_sum%% *}"
    if [ "$either_hash" != "$2" ] && [ "$either_hash" != "$3" ]; then
        printf '%s\nhas SHA-256 %s\nwanted      %s or %s\n' "$1" "$either_hash" "$2" "$3" \
            >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

expect_either_copy() {
    resolve_path "$1"
    wait_until 15 "$1 has one of the two copies" hash_is_either "$resolved_path" "$2" "$3"
    picked_hash="$either_hash"
}

# Every file below DIR is below the mount with the same content. Names with a space need
# the loop to read whole lines.
expect_hashes_from() {
    list_files "$1" >"$work/files.hash"
    while IFS= read -r hashed_file; do
        want="$(hash_of "$1/$hashed_file")"
        expect_hash "$share/$hashed_file" "$want"
    done <"$work/files.hash"
}

# The entries of a folder (files and folders) are exactly the words of NAMES, sorted. A name
# with a space cannot be written in NAMES; such a scenario compares with expect_same_tree.
names_are() {
    printf '%s' "$2" | tr ' ' '\n' | LC_ALL=C sort >"$work/names.want"
    if [ ! -d "$1" ]; then
        printf 'not a folder: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    find "$1" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | LC_ALL=C sort >"$work/names.got"
    diff -u "$work/names.want" "$work/names.got" >"$work/check.txt" 2>&1
}

# expect_names REL_DIR "a b c" [SECONDS]
expect_names() {
    resolve_path "$1"
    wait_until "${3:-15}" "the folder $1 holds exactly: $2" names_are "$resolved_path" "$2"
}

name_is_gone() {
    if [ -e "$1" ]; then
        printf 'still there: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# expect_missing REL [SECONDS]: the kernel keeps names for 30 s, so it waits up to 45 s.
expect_missing() {
    resolve_path "$1"
    wait_until "${2:-45}" "$1 is gone" name_is_gone "$resolved_path"
}

is_folder() {
    if [ ! -d "$1" ]; then
        printf 'not a folder: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

is_regular_file() {
    if [ ! -f "$1" ]; then
        printf 'not a file: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# expect_is_dir REL [SECONDS] and expect_is_file REL [SECONDS]
expect_is_dir() {
    resolve_path "$1"
    wait_until "${2:-15}" "$1 is a folder" is_folder "$resolved_path"
}

expect_is_file() {
    resolve_path "$1"
    wait_until "${2:-15}" "$1 is a file" is_regular_file "$resolved_path"
}

# expect_read_error REL TEXT [SECONDS]: reading the whole file fails, within SECONDS (60), and
# the error that cat printed contains TEXT. One read, no waiting for it to start failing.
expect_read_error() {
    resolve_path "$1"
    read_limit="${3:-60}"
    read_status=0
    timeout -k 5 "$read_limit" cat "$resolved_path" >/dev/null 2>"$work/check.txt" || read_status=$?
    [ "$read_status" -ne 0 ] || fail "reading $1 worked, wanted an error with: $2"
    [ "$read_status" -ne 124 ] || fail "reading $1 did not end in $read_limit s"
    grep -Fq -- "$2" "$work/check.txt" || fail "reading $1 failed without the text: $2"
    : >"$work/check.txt"
}

size_is() {
    if [ ! -e "$1" ]; then
        printf 'missing: %s\n' "$1" >"$work/check.txt"
        return 1
    fi
    size_now="$(stat -c %s "$1" 2>"$work/check.txt")" || return 1
    if [ "$size_now" != "$2" ]; then
        printf '%s\nhas %s bytes\nwanted %s bytes\n' "$1" "$size_now" "$2" >"$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# expect_size REL BYTES [SECONDS]: a changed size is seen after the kernel's 30 s of cached
# attributes, so it waits up to 45 s.
expect_size() {
    resolve_path "$1"
    wait_until "${3:-45}" "$1 has $2 bytes" size_is "$resolved_path" "$2"
}

# slow_read_start REL OUT FIRST_MIB PAUSE_SECONDS: a reader in the background copies the first
# FIRST_MIB mebibytes of the file to OUT, sleeps, then copies the rest from the same open
# file. The scenario changes things during the sleep. The file stays open in the reader, so
# the rest is read through the same handle. Its errors go to slow.txt, which the failure
# report shows.
slow_read_start() {
    resolve_path "$1"
    slow_source="$resolved_path"
    slow_out="$2"
    slow_first=$(($3 * 1024 * 1024))
    slow_pause="$4"
    : >"$work/slow.txt"
    (
        # The reader must not run the cleanup of this script, which removes the work folder.
        trap - EXIT INT TERM
        exec 4<"$slow_source"
        head -c "$slow_first" <&4 >"$slow_out"
        sleep "$slow_pause"
        cat <&4 >>"$slow_out"
    ) 2>"$work/slow.txt" &
    slow_pid=$!
}

# Waits for the reader of slow_read_start (at most 120 s) and fails the scenario when it
# failed. The caller compares the hash of OUT.
slow_read_wait() {
    [ -n "$slow_pid" ] || fail "slow_read_wait without slow_read_start"
    wait_until 120 "the slow reader ends" process_ended "$slow_pid"
    slow_status=0
    wait "$slow_pid" || slow_status=$?
    slow_pid=""
    [ "$slow_status" -eq 0 ] || fail "the slow reader failed with status $slow_status"
}

# The file NAME of the share was fetched from at least MIN servers. A server logs one line
# per request (text format, which make_server sets): method, path, status. Run this after
# the file was read, because a read is what makes the client fetch.
expect_served_by_servers() {
    served=0
    for served_number in 1 2 3 4 5; do
        if grep -Eqs "method=GET path=/api/v1/file/media/$1 status=(200|206)( |\$)" \
            "$work/server$served_number.log"; then
            served=$((served + 1))
        fi
    done
    [ "$served" -ge "$2" ] || fail "$1 was fetched from $served servers, wanted at least $2"
}

# scenario NAME FUNCTION registers a scenario. The function must exist already, so that a
# typo stops the run before the first scenario and not in the middle of it.
scenario() {
    command -v "$2" >/dev/null 2>&1 || fail "scenario \"$1\": the function $2 is not defined"
    printf '%s %s\n' "$2" "$1" >>"$scenario_list"
}

# Runs the registered scenarios in the order of registration. CLUSTER_ONLY=text keeps those
# whose name contains the text; none left is an error, because a typo there would otherwise
# look like a pass. The list is read on file descriptor 3 so that no scenario step can
# consume it through standard input.
run_scenarios() {
    only="${CLUSTER_ONLY:-}"
    scenarios_total=0
    scenarios_ran=0
    while IFS= read -r registered <&3; do
        scenarios_total=$((scenarios_total + 1))
        registered_name="${registered#* }"
        # The text may also be part of the function name, which starts with the prefix of
        # its file, so one group of scenarios can be run by its prefix (for example "trees_").
        case "$registered_name ${registered%% *}" in
            *"$only"*) ;;
            *) continue ;;
        esac
        begin_scenario "$registered_name"
        "${registered%% *}"
        end_scenario
        scenarios_ran=$((scenarios_ran + 1))
    done 3<"$scenario_list"
    [ "$scenarios_total" -gt 0 ] || fail "no scenario is registered"
    [ "$scenarios_ran" -gt 0 ] || fail "no scenario name contains CLUSTER_ONLY=$only"
    if [ -n "$only" ]; then
        echo "cluster: ran $scenarios_ran of $scenarios_total scenarios (CLUSTER_ONLY=$only)"
    fi
}
