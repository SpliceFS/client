# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenarios in which the servers change while the share is
# mounted: a file is added, removed from one server, removed from every server, replaced, or
# changed in the middle of a read. The client has no background refresh, so these show that a
# listing is fetched again when something asks after listing_ttl, and that a file in the
# middle of a read is not mixed with the new copy. work, share and the time_* variables come
# from cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# Scratch folder for the copies that a scenario puts on the servers after the mount is up.
live_scratch="$work/live"

# Every scenario here uses a listing time of 1 s, so that a change on the servers is seen after
# 1 s and not after the 30 s default. Servers 1 and 2 run, node 3 is in the list but idle.
# The scenario puts its files in the data folders before this runs.
live_start() {
    client_setting listing_ttl 1s
    write_idle_node 3
    for live_n in 1 2; do start_server "$live_n"; done
    start_client
}

# True when the file at $1 holds at least $2 bytes. The slow reader writes its first part in
# one go, so this tells the scenario that the read is now in its pause.
live_has_bytes() {
    live_size="$(stat -c %s "$1" 2>/dev/null)" || return 1
    [ "$live_size" -ge "$2" ]
}

# Rule: a name that appears on a running server after the mount is up shows in the folder
# listing once the listing time has passed, and its content is right.
live_file_added() {
    put_text "$work/data1" base.txt 'the file that is there from the start'
    live_start
    expect_names . "base.txt"
    put_text "$work/data2" new.txt 'added to server 2 while the share is mounted'
    live_new_hash="$(hash_of "$work/data2/new.txt")"
    expect_names . "base.txt new.txt" 15
    expect_hash new.txt "$live_new_hash"
}
scenario "a file is added while mounted" live_file_added

# Rule: a file that one server loses is still listed and still read, because the other server
# has it. read.bin was read before the loss, so the cache can answer for it; fresh.bin was
# never read, so its bytes must come from server 2.
live_removed_from_one() {
    put_random "$work/data1" read.bin $((100 * 1024))
    stamp "$work/data1" read.bin "$time_old"
    put_random "$work/data1" fresh.bin $((100 * 1024))
    stamp "$work/data1" fresh.bin "$time_old"
    put_copy "$work/data1/read.bin" 2 read.bin
    put_copy "$work/data1/fresh.bin" 2 fresh.bin
    live_read_hash="$(hash_of "$work/data1/read.bin")"
    live_fresh_hash="$(hash_of "$work/data1/fresh.bin")"
    live_start
    expect_hash read.bin "$live_read_hash"
    remove_from 1 read.bin
    remove_from 1 fresh.bin
    # The listing time is 1 s, so after 3 s the next request lists the folder again and sees
    # server 1 without the files.
    sleep 3
    expect_names . "fresh.bin read.bin"
    expect_hash read.bin "$live_read_hash"
    expect_hash fresh.bin "$live_fresh_hash"
}
scenario "a file is removed from one server while mounted" live_removed_from_one

# Rule: a file that no running server has any more disappears from the listing, and opening it
# fails with "No such file or directory". It was read before, so the name was known to the
# kernel and the cache held its bytes; neither may keep it alive.
live_removed_from_all() {
    put_random "$work/data1" gone.bin $((100 * 1024))
    stamp "$work/data1" gone.bin "$time_old"
    put_copy "$work/data1/gone.bin" 2 gone.bin
    put_text "$work/data1" keep.txt 'this file stays'
    put_copy "$work/data1/keep.txt" 2 keep.txt
    live_gone_hash="$(hash_of "$work/data1/gone.bin")"
    live_start
    expect_hash gone.bin "$live_gone_hash"
    remove_from 1 gone.bin
    remove_from 2 gone.bin
    expect_missing gone.bin
    expect_read_error gone.bin "No such file"
    expect_names . "keep.txt"
}
scenario "a file is removed from every server while mounted" live_removed_from_all

# Rule: a file that is replaced on every server by a copy with another size and a newer time
# is seen as the new copy: the size changes first (the kernel keeps attributes for 30 s), then
# the content is the new one, not the cached old one.
live_replaced_everywhere() {
    put_random "$work/data1" doc.bin $((100 * 1024))
    stamp "$work/data1" doc.bin "$time_old"
    put_copy "$work/data1/doc.bin" 2 doc.bin
    live_old_hash="$(hash_of "$work/data1/doc.bin")"
    live_start
    expect_hash doc.bin "$live_old_hash"
    put_random "$live_scratch" doc.bin $((150 * 1024))
    stamp "$live_scratch" doc.bin "$time_new"
    live_replaced_hash="$(hash_of "$live_scratch/doc.bin")"
    put_copy "$live_scratch/doc.bin" 1 doc.bin
    put_copy "$live_scratch/doc.bin" 2 doc.bin
    expect_size doc.bin $((150 * 1024)) 45
    expect_hash doc.bin "$live_replaced_hash" 45
    rm -rf "${live_scratch:?}"
}
scenario "a file is replaced on every server while mounted" live_replaced_everywhere

# Rule: a reader that has the file open gets one complete copy, the one it started with, even
# when one server overwrites its copy with other bytes of the same size and a newer time in the
# middle of the read. The new bytes are made before the read starts, so that the overwrite in
# the pause of the reader is a quick copy and not 160 MiB of random data.
live_changes_during_read() {
    live_big_size=$((160 * 1024 * 1024))
    put_random "$work/data1" big.bin "$live_big_size"
    stamp "$work/data1" big.bin "$time_old"
    put_copy "$work/data1/big.bin" 2 big.bin
    live_original_hash="$(hash_of "$work/data1/big.bin")"
    put_random "$live_scratch" big.bin "$live_big_size"
    stamp "$live_scratch" big.bin "$time_new"
    live_start
    slow_read_start big.bin "$live_scratch/out.bin" 8 3
    wait_until 30 "the reader has copied its first 8 MiB" live_has_bytes \
        "$live_scratch/out.bin" $((8 * 1024 * 1024))
    put_copy "$live_scratch/big.bin" 2 big.bin
    slow_read_wait
    live_read_result="$(hash_of "$live_scratch/out.bin")"
    [ "$live_read_result" = "$live_original_hash" ] ||
        fail "the copy that was read has SHA-256 $live_read_result, wanted that of the original file, $live_original_hash"
    rm -rf "${live_scratch:?}"
}
scenario "a file changes on one server in the middle of a read" live_changes_during_read
