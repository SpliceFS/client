# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenarios about one name held with different contents: the
# version most servers hold wins, on a tie the newest time wins, and with the same size and
# time one copy wins by a fixed order. work, share and the time_* variables come from
# cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# doc-a.txt is newer on server 1, doc-b.txt on server 2, and the texts differ in length. The
# newer copy wins whichever server holds it.
versions_expect_newest_copies() {
    expect_hash "$share/doc-a.txt" "$1"
    expect_hash "$share/doc-b.txt" "$2"
}

# Rule: on a tie of votes (one server each) the newest modification time wins, and it is the
# same copy after a restart and after the cache is emptied.
versions_newest_wins() {
    put_text "$work/data1" doc-a.txt 'doc a as server 1 has it, the newer copy of this file'
    stamp "$work/data1" doc-a.txt "$time_new"
    put_text "$work/data2" doc-a.txt 'older doc a'
    stamp "$work/data2" doc-a.txt "$time_old"
    put_text "$work/data1" doc-b.txt 'older doc b'
    stamp "$work/data1" doc-b.txt "$time_old"
    put_text "$work/data2" doc-b.txt 'doc b as server 2 has it, the newer copy of this file'
    stamp "$work/data2" doc-b.txt "$time_new"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    hash_a="$(hash_of "$work/data1/doc-a.txt")"
    hash_b="$(hash_of "$work/data2/doc-b.txt")"
    versions_expect_newest_copies "$hash_a" "$hash_b"
    # Again in the same mount, with the cache kept, and with the cache emptied: it is the
    # same copy every time.
    versions_expect_newest_copies "$hash_a" "$hash_b"
    restart_client
    versions_expect_newest_copies "$hash_a" "$hash_b"
    restart_client_empty_cache
    versions_expect_newest_copies "$hash_a" "$hash_b"
}
scenario "same name, different contents, the newest wins" versions_newest_wins

# Rule: with the same size and time but different bytes, one copy wins by a fixed order, the
# same every time.
versions_same_size_and_time() {
    put_random "$work/data1" same.bin $((1024 * 1024))
    stamp "$work/data1" same.bin "$time_mid"
    put_random "$work/data2" same.bin $((1024 * 1024))
    stamp "$work/data2" same.bin "$time_mid"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    hash_one="$(hash_of "$work/data1/same.bin")"
    hash_two="$(hash_of "$work/data2/same.bin")"
    # Which copy wins is fixed by the client's order, so it is not named here; it must be one
    # of the two, and the same one every time.
    expect_either_copy "$share/same.bin" "$hash_one" "$hash_two"
    first_hash="$picked_hash"
    expect_hash "$share/same.bin" "$first_hash"
    restart_client
    expect_hash "$share/same.bin" "$first_hash"
    restart_client_empty_cache
    expect_hash "$share/same.bin" "$first_hash"
}
scenario "same name, same size and time, different contents" versions_same_size_and_time

# Rule: the version that most servers hold wins, even against a newer time on one server.
versions_most_servers_win() {
    put_text "$work/data1" vote.txt 'the text that two servers hold'
    stamp "$work/data1" vote.txt "$time_old"
    cp -p "$work/data1/vote.txt" "$work/data2/vote.txt"
    put_text "$work/data3" vote.txt 'another text, held by one server only, with the newer time'
    stamp "$work/data3" vote.txt "$time_new"
    for n in 1 2 3; do start_server "$n"; done
    start_client
    hash_two_servers="$(hash_of "$work/data1/vote.txt")"
    expect_hash "$share/vote.txt" "$hash_two_servers"
}
scenario "same name, different contents, most servers win" versions_most_servers_win

# Rule: when the server with the newer copy is gone, a new mount serves what the running
# servers agree on, which is the copy of server 1.
versions_chosen_server_stops() {
    put_text "$work/data1" gone.txt 'the older copy on server 1'
    stamp "$work/data1" gone.txt "$time_old"
    put_text "$work/data2" gone.txt 'the newer copy on server 2, with a longer text'
    stamp "$work/data2" gone.txt "$time_new"
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    gone_one="$(hash_of "$work/data1/gone.txt")"
    gone_two="$(hash_of "$work/data2/gone.txt")"
    expect_hash "$share/gone.txt" "$gone_two"
    stop_client
    stop_server 2
    empty_cache
    start_client
    expect_hash "$share/gone.txt" "$gone_one"
}
scenario "the server with the chosen copy stops" versions_chosen_server_stops
