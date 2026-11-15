# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenarios that need no version conflict: servers that hold
# the same files, different files, or files on one server only. work, share and the time_*
# variables come from cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from
# this file alone.
# shellcheck disable=SC2154

# Rule: with every server holding the same files, the mount lists them once, and a large file
# is fetched from more than one server.
basics_three_servers_same_files() {
    for n in 1 2 3; do
        copy_source "$n"
        copy_large "$n"
    done
    for n in 1 2 3; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data1"
    expect_hashes_from "$work/source"
    # Reading large.bin completely is what makes the client fetch its blocks.
    expect_hashes_from "$work/source-large"
    expect_served_by_servers large.bin 2
}
scenario "three servers, the same files" basics_three_servers_same_files

# Rule: a server that is in the node list but not running does not break the listing.
basics_two_running_same_files() {
    for n in 1 2; do copy_source "$n"; done
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data1"
    expect_hashes_from "$work/source"
}
scenario "three in the list, two running, the same files" basics_two_running_same_files

# Rule: the folder listing is the union of what the running servers hold.
basics_two_running_different_files() {
    put_text "$work/data1" a.txt 'only server 1 has this'
    put_random "$work/data1" only-one/x.bin $((1024 * 1024))
    put_text "$work/data2" b.txt 'only server 2 has this'
    put_random "$work/data2" only-two/y.bin $((1024 * 1024))
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    # The four names are all different, so the union is the copy of both folders.
    rm -rf "$work/union"
    mkdir -p "$work/union"
    cp -pR "$work/data1/." "$work/data2/." "$work/union/"
    expect_same_tree "$work/union"
    expect_hashes_from "$work/data1"
    expect_hashes_from "$work/data2"
}
scenario "three in the list, two running, different files" basics_two_running_different_files

# Rule: a file that only one running server holds is served by that server.
basics_two_running_files_on_one() {
    copy_source 2
    write_idle_node 3
    for n in 1 2; do start_server "$n"; done
    start_client
    expect_same_tree "$work/data2"
    expect_hashes_from "$work/source"
}
scenario "three in the list, two running, files on one only" basics_two_running_files_on_one
