# shellcheck shell=sh
# Sourced by cluster-test.sh. More scenarios about one name held in different versions (a
# version is a size and a modification time): the version most servers hold wins, on a tie the
# newest time wins. Here are the cases around that rule: three servers, three versions, equal
# bytes with different times, empty files, a copy cut short, and a file that only one server
# has in a folder that another server also has. work, share and the time_* variables come from
# cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# Starts the servers whose numbers are given, one after the other.
more_start_servers() {
    for more_number in "$@"; do start_server "$more_number"; done
}

# Rule: the version that most servers hold wins. Here it is also the newest, so the scenario
# shows that two matching copies are served as one version, not as two competing ones.
more_newer_copy_on_most_servers() {
    put_text "$work/data1" f.txt 'the old text, held by server 1 only'
    stamp "$work/data1" f.txt "$time_old"
    put_text "$work/data2" f.txt 'the new text that servers 2 and 3 hold, a little longer'
    stamp "$work/data2" f.txt "$time_new"
    put_copy "$work/data2/f.txt" 3 f.txt
    more_start_servers 1 2 3
    start_client
    more_new_hash="$(hash_of "$work/data2/f.txt")"
    expect_hash f.txt "$more_new_hash"
}
scenario "the newer copy is on most servers" more_newer_copy_on_most_servers

# Rule: when every server holds its own version (one vote each), the newest modification time
# wins. The newest copy is on server 2, so the pick is not simply the first or the last server.
more_three_copies() {
    put_text "$work/data1" three.txt 'the oldest copy of the text, on server 1'
    stamp "$work/data1" three.txt "$time_old"
    put_text "$work/data2" three.txt 'the newest copy of the text, on server 2, with a different length'
    stamp "$work/data2" three.txt "$time_new"
    put_text "$work/data3" three.txt 'the middle copy, on server 3'
    stamp "$work/data3" three.txt "$time_mid"
    more_start_servers 1 2 3
    start_client
    more_newest_hash="$(hash_of "$work/data2/three.txt")"
    expect_hash three.txt "$more_newest_hash"
}
scenario "three servers, three copies" more_three_copies

# Rule: two copies with equal bytes but different times are two versions. The newest one is
# listed, with its size, and it reads as the one content that both servers hold.
more_same_content_other_times() {
    more_size=$((100 * 1024))
    put_random "$work/data1" same.bin "$more_size"
    stamp "$work/data1" same.bin "$time_old"
    put_copy "$work/data1/same.bin" 2 same.bin
    stamp "$work/data2" same.bin "$time_new"
    write_idle_node 3
    more_start_servers 1 2
    start_client
    more_same_hash="$(hash_of "$work/data1/same.bin")"
    expect_hash same.bin "$more_same_hash"
    expect_size same.bin "$more_size"
}
scenario "the same content with different times" more_same_content_other_times

# Rule: an empty file is a version like any other. On a tie of votes the newest time wins even
# when the winner is empty, and a newer full copy wins against an older empty one.
more_empty_and_full() {
    more_full_size=$((200 * 1024))
    put_random "$work/data1" a.bin 0
    stamp "$work/data1" a.bin "$time_new"
    put_random "$work/data2" a.bin "$more_full_size"
    stamp "$work/data2" a.bin "$time_old"
    put_random "$work/data1" b.bin 0
    stamp "$work/data1" b.bin "$time_old"
    put_random "$work/data2" b.bin "$more_full_size"
    stamp "$work/data2" b.bin "$time_new"
    write_idle_node 3
    more_start_servers 1 2
    start_client
    more_empty_hash="$(hash_of "$work/data1/a.bin")"
    more_full_hash="$(hash_of "$work/data2/b.bin")"
    expect_size a.bin 0
    expect_hash a.bin "$more_empty_hash"
    expect_size b.bin "$more_full_size"
    expect_hash b.bin "$more_full_hash"
}
scenario "an empty copy and a full copy" more_empty_and_full

# Rule: the client trusts size and time only. A hazard follows from it: a transfer that wrote
# in place and stopped leaves a short file with a NEW time. With two servers that is one vote
# against one and the newest time wins, so the cut-short copy hides the complete one. With a
# third server that holds the complete copy, two votes beat the newer short copy.
more_cut_short_copy() {
    more_mib=$((1024 * 1024))
    put_random "$work/data1" movie.bin $((3 * more_mib))
    stamp "$work/data1" movie.bin "$time_old"
    head -c "$more_mib" "$work/data1/movie.bin" >"$work/data2/movie.bin"
    stamp "$work/data2" movie.bin "$time_new"
    more_full_hash="$(hash_of "$work/data1/movie.bin")"
    more_short_hash="$(hash_of "$work/data2/movie.bin")"
    write_idle_node 3
    more_start_servers 1 2
    start_client
    expect_size movie.bin "$more_mib"
    expect_hash movie.bin "$more_short_hash"
    # A new mount with an empty cache, so that nothing of the short copy is remembered.
    stop_client
    empty_cache
    put_copy "$work/data1/movie.bin" 3 movie.bin
    start_server 3
    start_client
    expect_size movie.bin $((3 * more_mib))
    expect_hash movie.bin "$more_full_hash"
}
scenario "a copy cut short by a failed transfer" more_cut_short_copy

# Rule: a folder that exists on two servers lists the union of both, so a file that only
# server 2 has is listed and read although server 1 has the same folder without it.
more_file_in_a_folder_of_another_server() {
    put_empty_dir 1 docs
    put_text "$work/data2" docs/only.txt 'this file is on server 2 only'
    write_idle_node 3
    more_start_servers 1 2
    start_client
    more_only_hash="$(hash_of "$work/data2/docs/only.txt")"
    expect_names docs "only.txt"
    expect_is_file docs/only.txt
    expect_hash docs/only.txt "$more_only_hash"
}
scenario "one server has the file, the other an older listing of nothing" \
    more_file_in_a_folder_of_another_server
