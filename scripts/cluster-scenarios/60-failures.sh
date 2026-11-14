# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenarios about servers that fail or are not what the client
# expects: a server that stops (and comes back) in the middle of a read, a cluster host that is
# down at the start or stops later, every server gone, a server without the share, and a node
# list with servers that never run. work, share and the time_* variables come from
# cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# The copy that slow_read_start writes. It is made new by every scenario that uses it.
fail_out_name="fail-read.bin"

# The size of the first part that the slow reader copies before its pause.
fail_first_mib=8

# True once the slow reader has copied its first part, which is when it starts to sleep. The
# scenario changes the servers then, so that the rest of the read meets the change.
fail_first_part_read() {
    fail_first_size="$(stat -c %s "$work/$fail_out_name" 2>/dev/null)" || return 1
    [ "$fail_first_size" -ge $((fail_first_mib * 1024 * 1024)) ]
}

# Servers 1, 2 and 3 hold the same 160 MiB file: 32 blocks of 5 MiB, so every server is asked
# for some of them and the read-ahead has blocks in flight when a server goes away.
fail_start_big_read() {
    put_random "$work/data1" big.bin $((160 * 1024 * 1024))
    stamp "$work/data1" big.bin "$time_mid"
    put_copy "$work/data1/big.bin" 2 big.bin
    put_copy "$work/data1/big.bin" 3 big.bin
    for fail_number in 1 2 3; do start_server "$fail_number"; done
    start_client
    slow_read_start big.bin "$work/$fail_out_name" "$fail_first_mib" 3
    wait_until 60 "the slow reader has copied its first part" fail_first_part_read
}

# Waits for the slow reader and compares what it copied with the original file.
fail_finish_big_read() {
    slow_read_wait
    fail_want="$(hash_of "$work/data1/big.bin")"
    fail_got="$(hash_of "$work/$fail_out_name")"
    [ "$fail_got" = "$fail_want" ] || fail "the copy read during the failure has SHA-256 $fail_got, wanted $fail_want"
    rm -f "$work/$fail_out_name"
}

# Rule: a server that stops while a file is read costs nothing but speed. The blocks it was asked
# for are fetched from the other servers, and every byte is right.
fail_server_stops_in_read() {
    fail_start_big_read
    stop_server 3
    fail_finish_big_read
}
scenario "a server stops in the middle of a read" fail_server_stops_in_read

# Rule: a server that stops and is back a second later does not break the read either, whether
# the client sees it gone, or back, or both.
fail_server_restarts_in_read() {
    fail_start_big_read
    stop_server 2
    sleep 1
    start_server 2
    fail_finish_big_read
}
scenario "a server stops and comes back in the middle of a read" fail_server_restarts_in_read

# start_client waits for a listing, which cannot work here. This starts the client the same way
# and waits only until the mount point is a mount.
fail_mount_exists() {
    mountpoint -q "$work/mnt"
}

fail_start_client_unlisted() {
    make_client_config
    "$client_bin" --config "$work/c" mount >"$work/client.log" 2>&1 &
    client_pid=$!
    watch_pid="$client_pid"
    wait_until 30 "the mount at $work/mnt exists" fail_mount_exists
    # wait_until in cluster-lib.sh reads watch_pid, which ShellCheck cannot see from this file.
    # shellcheck disable=SC2034
    watch_pid=""
}

# Listing the share fails now: within 20 s, and with any error.
fail_listing_fails() {
    fail_list_status=0
    timeout -k 5 20 ls "$share" >/dev/null 2>"$work/check.txt" || fail_list_status=$?
    [ "$fail_list_status" -ne 0 ] || fail "listing $share worked while server 1 is down"
    [ "$fail_list_status" -ne 124 ] || fail "listing $share did not end in 20 s"
    : >"$work/check.txt"
}

# Rule: with the cluster host down at the start the mount comes up, listings fail, and the
# client recovers by itself when the host is back (its retries wait at most 60 s).
fail_first_server_down_at_start() {
    put_random "$work/data1" one.bin 65536
    put_random "$work/data2" two.bin 65536
    fail_one="$(hash_of "$work/data1/one.bin")"
    fail_two="$(hash_of "$work/data2/two.bin")"
    start_server 2
    fail_start_client_unlisted
    fail_listing_fails
    start_server 1
    wait_until 70 "the share can be listed after server 1 is up" share_listable
    expect_names . "one.bin two.bin"
    expect_hash one.bin "$fail_one"
    expect_hash two.bin "$fail_two"
}
scenario "the first server is down at the start" fail_first_server_down_at_start

# Rule: the cluster host is needed to log in, not to go on. After a successful start the client
# keeps using the other servers, also for a file it has never read.
fail_first_server_stops_after_start() {
    put_random "$work/data1" first.bin 262144
    put_copy "$work/data1/first.bin" 2 first.bin
    put_random "$work/data1" second.bin 262144
    put_copy "$work/data1/second.bin" 2 second.bin
    fail_first="$(hash_of "$work/data1/first.bin")"
    fail_second="$(hash_of "$work/data1/second.bin")"
    for fail_number in 1 2; do start_server "$fail_number"; done
    start_client
    expect_hash first.bin "$fail_first"
    stop_server 1
    expect_hash second.bin "$fail_second" 30
}
scenario "the first server stops after the start" fail_first_server_stops_after_start

# Rule: with every server down, a file that is completely in the cache (and whose folder was
# listed in this mount) is still read, and a file that is not in the cache fails with an
# input/output error and does not hang.
fail_every_server_stops() {
    put_random "$work/data1" cached.bin $((2 * 1024 * 1024))
    put_copy "$work/data1/cached.bin" 2 cached.bin
    put_random "$work/data1" never.bin $((2 * 1024 * 1024))
    put_copy "$work/data1/never.bin" 2 never.bin
    fail_cached="$(hash_of "$work/data1/cached.bin")"
    for fail_number in 1 2; do start_server "$fail_number"; done
    start_client
    expect_hash cached.bin "$fail_cached"
    # Looking the name up is not reading it, and it keeps the name in the kernel for the check
    # below, so that the error comes from the missing bytes and not from a failed listing.
    expect_is_file never.bin
    stop_server 1
    stop_server 2
    expect_hash cached.bin "$fail_cached"
    expect_read_error never.bin "Input/output error" 60
}
scenario "every server stops" fail_every_server_stops

# Rule: a server that runs but offers no share adds nothing and breaks nothing. The mount shows
# the files of the server that has the share, and every hash is right.
fail_server_without_share() {
    put_text "$work/data1" readme.txt 'the share is on server 1 only'
    put_random "$work/data1" docs/a.bin 100000
    put_random "$work/data1" docs/deep/b.bin 65536
    put_random "$work/data1" other/c.bin 4096
    make_server_without_share 2
    for fail_number in 1 2; do start_server "$fail_number"; done
    start_client
    expect_same_tree "$work/data1"
    expect_hashes_from "$work/data1"
}
scenario "one server does not have the share" fail_server_without_share

# Rule: servers in the node list that never run (three of five here) cost nothing but a refused
# connection, and the mount shows the union of what the two running servers hold.
fail_two_servers_in_five() {
    for fail_number in 3 4 5; do write_idle_node "$fail_number"; done
    put_text "$work/data1" left/a.txt 'a file only server 1 holds'
    put_random "$work/data1" left/deep/b.bin 50000
    put_text "$work/data1" joint/one.txt 'server 1 in a shared folder'
    put_text "$work/data2" right/c.txt 'a file only server 2 holds'
    put_random "$work/data2" right/deep/d.bin 70000
    put_text "$work/data2" joint/two.txt 'server 2 in a shared folder'
    for fail_number in 1 2; do start_server "$fail_number"; done
    start_client
    expect_names . "joint left right"
    expect_names joint "one.txt two.txt"
    expect_hashes_from "$work/data1"
    expect_hashes_from "$work/data2"
}
scenario "two servers in a list of five" fail_two_servers_in_five
