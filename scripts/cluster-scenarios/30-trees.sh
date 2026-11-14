# shellcheck shell=sh
# Sourced by cluster-test.sh. The scenarios about folder layouts: trees that do not overlap
# or overlap in part, one name that is a file on some servers and a folder on others, empty
# folders, unusual names, names the server hides, and a folder with many files. The rule
# under all of them: a folder listing is the union of what the running servers hold. work
# and share come from cluster-test.sh and cluster-lib.sh, which ShellCheck cannot see from
# this file alone.
# shellcheck disable=SC2154

# The files of the data folders of the given servers as one folder. The mount must show the
# same files with the same bytes. Only for scenarios where no name is a file on one server and
# a folder on another, and where equal names hold the same copy on every server (copying
# later over earlier files is then harmless).
trees_union() {
    rm -rf "${work:?}/trees-union"
    mkdir -p "$work/trees-union"
    for trees_server in "$@"; do
        cp -pR "$work/data$trees_server/." "$work/trees-union/"
    done
}

# The mount holds exactly the files of the union of the given servers, and every file has the
# bytes of its copy in the union.
trees_expect_union() {
    trees_union "$@"
    expect_same_tree "$work/trees-union"
    expect_hashes_from "$work/trees-union"
}

# Rule: trees that share no name are both shown, whole. Server 1 and 2 run, 3 is idle.
trees_no_overlap() {
    put_text "$work/data1" alpha/one.txt 'alpha one, on server 1'
    put_text "$work/data1" alpha/sub/two.txt 'alpha sub two, on server 1'
    put_random "$work/data1" alpha/sub/three.bin 20000
    put_text "$work/data2" beta/a.txt 'beta a, on server 2'
    put_text "$work/data2" beta/b/c.txt 'beta b c, on server 2'
    put_random "$work/data2" beta/b/d.bin 30000
    write_idle_node 3
    for trees_server in 1 2; do start_server "$trees_server"; done
    start_client
    expect_names . "alpha beta"
    expect_names alpha "one.txt sub"
    expect_names alpha/sub "three.bin two.txt"
    expect_names beta "a.txt b"
    expect_names beta/b "c.txt d.bin"
    trees_expect_union 1 2
}
scenario "trees that do not overlap" trees_no_overlap

# Rule: where two trees share a folder, the folder lists the files of both, and a file that
# both hold as the same copy is listed once. Folders only one server has are merged in at
# every level. Server 1 and 2 run, 3 is idle.
trees_partly_overlap() {
    put_text "$work/data1" common/shared.txt 'the same copy on both servers'
    put_copy "$work/data1/common/shared.txt" 2 common/shared.txt
    put_text "$work/data1" common/one.txt 'only on server 1'
    put_random "$work/data1" common/deep/a/b/one.bin 5000
    put_text "$work/data1" top1.txt 'top level, only on server 1'
    put_text "$work/data2" common/two.txt 'only on server 2'
    put_random "$work/data2" common/deep/a/c/two.bin 7000
    put_text "$work/data2" top2.txt 'top level, only on server 2'
    write_idle_node 3
    for trees_server in 1 2; do start_server "$trees_server"; done
    start_client
    expect_names . "common top1.txt top2.txt"
    expect_names common "deep one.txt shared.txt two.txt"
    expect_names common/deep "a"
    expect_names common/deep/a "b c"
    expect_names common/deep/a/b "one.bin"
    expect_names common/deep/a/c "two.bin"
    trees_expect_union 1 2
}
scenario "trees that partly overlap" trees_partly_overlap

# Rule: a name that is a file on some servers and a folder on others takes the kind most
# running servers report; a tie becomes a folder. Part one: thing is a file on server 1 and a
# folder on server 2, one each, so it is a folder. Part two, with server 3 running too: other
# is a file on 1 and 2 and a folder on 3, so it is a file; thing is still a tie, still a
# folder. Server 3 runs once at the start and stops, so that it is in the node list while it
# is down in part one, as the other scenarios do with an idle node.
trees_file_or_folder() {
    put_text "$work/data1" thing 'thing is a file on server 1'
    put_text "$work/data2" thing/inside.txt 'inside the folder thing, on server 2'
    put_text "$work/data1" other 'other is a file on servers 1 and 2'
    put_copy "$work/data1/other" 2 other
    put_text "$work/data3" other/inner.txt 'inner, in the folder other on server 3'
    trees_inside_hash="$(hash_of "$work/data2/thing/inside.txt")"
    trees_other_hash="$(hash_of "$work/data1/other")"
    for trees_server in 1 2 3; do start_server "$trees_server"; done
    stop_server 3
    start_client
    expect_names . "other thing"
    expect_is_dir thing
    expect_names thing "inside.txt"
    expect_hash thing/inside.txt "$trees_inside_hash"
    # Part two. The cache is emptied so that no listing of part one is reused.
    start_server 3
    restart_client_empty_cache
    expect_names . "other thing"
    expect_is_file other
    expect_hash other "$trees_other_hash"
    expect_is_dir thing
    expect_hash thing/inside.txt "$trees_inside_hash"
}
scenario "a file on one server, a folder on the other" trees_file_or_folder

# Rule: a folder that is empty on one server is listed as an empty folder, next to the files
# of another server. Server 1 and 2 run, 3 is idle.
trees_empty_folder() {
    put_empty_dir 2 empty-dir
    put_text "$work/data1" full/x.txt 'a file in a full folder on server 1'
    write_idle_node 3
    for trees_server in 1 2; do start_server "$trees_server"; done
    start_client
    expect_names . "empty-dir full"
    expect_is_dir empty-dir
    expect_names empty-dir ""
    expect_names full "x.txt"
    trees_x_hash="$(hash_of "$work/data1/full/x.txt")"
    expect_hash full/x.txt "$trees_x_hash"
}
scenario "an empty folder on one server" trees_empty_folder

# One file with the same copy on server 1 and 2: NAME, TEXT.
trees_put_both() {
    put_text "$work/data1" "$1" "$2"
    put_copy "$work/data1/$1" 2 "$1"
}

# Rule: names that need care at every layer (a space, bytes outside ASCII, a leading dash, the
# longest practical length, a single quote) are listed and read with the right bytes. The
# names are made with printf and octal escapes so that this file stays plain ASCII. Server 1
# and 2 run with the same copies, 3 is idle.
trees_unusual_names() {
    trees_cafe="$(printf 'caf\303\251.txt')"
    # 5 + 191 + 4 characters.
    trees_long="$(printf 'long-%0191d.txt' 0)"
    [ "${#trees_long}" -eq 200 ] || fail "the long name has ${#trees_long} characters, wanted 200"
    trees_put_both "with space.txt" 'the name has a space'
    trees_put_both "$trees_cafe" 'the name has two bytes outside ASCII'
    trees_put_both "-dash.txt" 'the name starts with a dash'
    trees_put_both "$trees_long" 'the name has 200 characters'
    trees_put_both "it's.txt" 'the name has a single quote'
    write_idle_node 3
    for trees_server in 1 2; do start_server "$trees_server"; done
    start_client
    # A name with a space cannot be given to expect_names; the same files check compares the
    # names exactly instead.
    trees_expect_union 1 2
    trees_count="$(find "$share" -mindepth 1 -maxdepth 1 | wc -l)"
    [ "$trees_count" -eq 5 ] || fail "the share lists $trees_count entries, wanted 5"
}
scenario "unusual names" trees_unusual_names

# Rule: the server hides names that start with a dot, symbolic links and names it does not
# serve as files of the share, so the mount lists only the plain files. Only server 1 runs,
# 2 and 3 are idle.
trees_hidden_names() {
    put_text "$work/data1" .hidden.txt 'a name that starts with a dot'
    put_text "$work/data1" .sync-tool.movie.tmp 'the temporary file of a transfer tool'
    put_text "$work/data1" real.txt 'the file the link points to'
    ln -s real.txt "$work/data1/link.txt"
    put_text "$work/data1" movie.part 'a name that only looks like a partial file'
    write_idle_node 2
    write_idle_node 3
    start_server 1
    start_client
    expect_names . "movie.part real.txt"
    trees_real_hash="$(hash_of "$work/data1/real.txt")"
    trees_movie_hash="$(hash_of "$work/data1/movie.part")"
    expect_hash real.txt "$trees_real_hash"
    expect_hash movie.part "$trees_movie_hash"
}
scenario "names the server hides" trees_hidden_names

# The hashes of the 300 files, read in one run over the names of the mount, are the hashes
# of the source files. Run once a second by wait_until, because the client may still be
# hearing from the servers. What differs is left in check.txt.
trees_many_hashes_match() {
    (cd "$share" && sha256sum -- f*) >"$work/trees-many.got" 2>"$work/check.txt" || return 1
    diff -u "$work/trees-many.want" "$work/trees-many.got" >"$work/check.txt" 2>&1
}

# Rule: a long folder is listed completely and not in a first page only: 300 names, of which
# 100 are on both servers as identical copies and are listed once. Server 1 and 2 run, 3 is
# idle.
trees_many_files() {
    rm -rf "${work:?}/trees-many"
    trees_names=""
    trees_number=1
    while [ "$trees_number" -le 300 ]; do
        trees_name="$(printf 'f%03d' "$trees_number")"
        put_random "$work/trees-many" "$trees_name" 1024
        if [ "$trees_number" -le 200 ]; then put_copy "$work/trees-many/$trees_name" 1 "$trees_name"; fi
        if [ "$trees_number" -ge 101 ]; then put_copy "$work/trees-many/$trees_name" 2 "$trees_name"; fi
        trees_names="${trees_names:+$trees_names }$trees_name"
        trees_number=$((trees_number + 1))
    done
    (cd "$work/trees-many" && sha256sum -- f*) >"$work/trees-many.want"
    trees_lines="$(wc -l <"$work/trees-many.want")"
    [ "$trees_lines" -eq 300 ] || fail "the hash list of the source has $trees_lines lines, wanted 300"
    write_idle_node 3
    for trees_server in 1 2; do start_server "$trees_server"; done
    start_client
    expect_names . "$trees_names"
    wait_until 30 "the 300 files of the mount have the hashes of the source files" trees_many_hashes_match
}
scenario "a folder with many files" trees_many_files
