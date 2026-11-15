# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenario about copies that agree in size, time and start
# and differ further in and at the end: the tail digest tells them apart, so a read never
# mixes them.
# work, share and the time_* variables come from cluster-test.sh and cluster-lib.sh, which
# ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# end.bin is 96 MiB on both servers, so its blocks are fetched from both. The copies have
# the same size, time and first 64 KiB. They differ in 4 KiB inside every 5 MiB block and
# in the last 4 KiB. If they were taken for one file, a read would take blocks from both
# servers, and since every block differs, the result would be neither copy. The tail
# digest tells them apart. Rule: one copy is used, the same one every time, never a mix.
tail_differ_only_at_the_end() {
    tail_size=$((96 * 1024 * 1024))
    put_random "$work/data1" end.bin "$tail_size"
    cp "$work/data1/end.bin" "$work/data2/end.bin"
    put_random "$work" tail-other.bin 4096
    # One megabyte into each block, so the first 64 KiB of the file stay equal. The file has
    # 19 whole blocks of 5 MiB and a last one of 1 MiB, which the write at the end covers; a
    # write one megabyte into that last block would land behind the end and make the file
    # longer, and then the two copies would differ in size and prove nothing here.
    tail_block=0
    while [ "$tail_block" -lt 19 ]; do
        dd if="$work/tail-other.bin" of="$work/data2/end.bin" bs=4096 \
            seek=$(((tail_block * 5 + 1) * 1024 * 1024 / 4096)) conv=notrunc 2>/dev/null
        tail_block=$((tail_block + 1))
    done
    dd if="$work/tail-other.bin" of="$work/data2/end.bin" bs=4096 \
        seek=$((tail_size / 4096 - 1)) conv=notrunc 2>/dev/null
    stamp "$work/data1" end.bin "$time_mid"
    stamp "$work/data2" end.bin "$time_mid"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    tail_one="$(hash_of "$work/data1/end.bin")"
    tail_two="$(hash_of "$work/data2/end.bin")"
    [ "$tail_one" != "$tail_two" ] || fail "the two copies of end.bin do not differ"
    tail_size_one="$(stat -c %s "$work/data1/end.bin")"
    tail_size_two="$(stat -c %s "$work/data2/end.bin")"
    [ "$tail_size_one" = "$tail_size_two" ] || fail "the two copies of end.bin differ in size"
    expect_either_copy "$share/end.bin" "$tail_one" "$tail_two"
    tail_first="$picked_hash"
    expect_hash "$share/end.bin" "$tail_first"
    restart_client
    expect_hash "$share/end.bin" "$tail_first"
    restart_client_empty_cache
    expect_hash "$share/end.bin" "$tail_first"
}
scenario "copies that differ after the start and at the end" tail_differ_only_at_the_end
