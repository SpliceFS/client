# shellcheck shell=sh
# Sourced by cluster-test.sh. A generated mix of files on three servers: the same path is held
# by some servers, in up to three versions, and the reader must show exactly what the merge
# rules give (the version most running servers hold wins, on a tie the newest time). Hand
# written cases cover one rule each; a generated plan covers the rules together, in many
# combinations that nobody thought of. work and share come from cluster-test.sh and
# cluster-lib.sh, which ShellCheck cannot see from this file alone.
# shellcheck disable=SC2154

# Everything of the plan lives in one folder, which is made again at the start of each
# scenario: plan.txt, content/ (one file per path and version) and the expectation files.
gen_dir="$work/gen"
gen_path_count=60
gen_seed=""
# The servers that run, as digits ("12"), for the diagnostics of a failed check.
gen_running=""

# Writes plan.txt: one line per path and holder, "NUMBER PATH SERVER VERSION SIZE EPOCH". The
# awk generator is seeded, so the same seed gives the same plan on the same awk program, and
# the seed is printed first so that a failure can be repeated with CLUSTER_SEED. A path has 1
# to 3 versions that differ in size and in time (the content differs because the sizes do), at
# least one holder, and a version is the same copy on every server that holds it. File names
# never equal a folder name, so no path is both a file and a folder.
gen_write_plan() {
    gen_seed="${CLUSTER_SEED:-20261114}"
    case "$gen_seed" in
        "" | *[!0-9]*) fail "CLUSTER_SEED must be a whole number, it is: $gen_seed" ;;
        *) ;;
    esac
    printf 'generated mix: seed %s, %s paths\n' "$gen_seed" "$gen_path_count"
    rm -rf "${work:?}/gen"
    mkdir -p "$gen_dir/content"
    awk -v seed="$gen_seed" -v paths="$gen_path_count" '
        BEGIN {
            srand(seed)
            base = 1767268800
            n1 = split("alpha beta gamma delta", level1, " ")
            n2 = split("one two three", level2, " ")
            n3 = split("x y", level3, " ")
            n4 = split("txt bin dat log", ext, " ")
            for (i = 1; i <= paths; i++) {
                depth = int(rand() * 4)
                dir = ""
                if (depth >= 1) dir = level1[1 + int(rand() * n1)] "/"
                if (depth >= 2) dir = dir level2[1 + int(rand() * n2)] "/"
                if (depth >= 3) dir = dir level3[1 + int(rand() * n3)] "/"
                path = sprintf("%sf%02d.%s", dir, i, ext[1 + int(rand() * n4)])
                holders = 0
                for (s = 1; s <= 3; s++) {
                    hold[s] = (rand() < 0.7)
                    holders += hold[s]
                }
                if (holders == 0) {
                    hold[1 + int(rand() * 3)] = 1
                    holders = 1
                }
                versions = 1 + int(rand() * 3)
                for (v = 1; v <= versions; v++) {
                    do {
                        if (rand() < 0.08) size[v] = 0
                        else size[v] = 1 + int(rand() * 65536)
                        clash = 0
                        for (w = 1; w < v; w++) if (size[w] == size[v]) clash = 1
                    } while (clash)
                    do {
                        epoch[v] = base + (1 + int(rand() * 300)) * 86400
                        clash = 0
                        for (w = 1; w < v; w++) if (epoch[w] == epoch[v]) clash = 1
                    } while (clash)
                }
                for (s = 1; s <= 3; s++) {
                    if (!hold[s]) continue
                    version = 1 + int(rand() * versions)
                    printf "%d %s %d %d %d %d\n", i, path, s, version, size[version], epoch[version]
                }
            }
        }' >"$gen_dir/plan.txt"
    [ -s "$gen_dir/plan.txt" ] || fail "the generator wrote no plan"
}

# Makes the files of the plan in the data folders. One content file per path and version is
# made (random bytes of the planned size) and given its time before the copies are made, and
# cp -p inside put_copy keeps size and time, so every holder of a version has an equal copy.
gen_make_files() {
    while read -r gen_index gen_path gen_server gen_version gen_size gen_epoch; do
        gen_content="c$gen_index-v$gen_version"
        if [ ! -f "$gen_dir/content/$gen_content" ]; then
            put_random "$gen_dir/content" "$gen_content" "$gen_size"
            stamp "$gen_dir/content" "$gen_content" "@$gen_epoch"
        fi
        put_copy "$gen_dir/content/$gen_content" "$gen_server" "$gen_path"
    done <"$gen_dir/plan.txt"
}

# gen_choose_versions RUNNING OUTFILE: what the reader must show when only the servers named
# in RUNNING (digits, "123") run. For each path that a running server holds, the version held
# by most running servers; on a tie the newest time. One line per path, "./PATH NUMBER
# VERSION SIZE". The same plan serves every set of running servers.
gen_choose_versions() {
    awk -v running="$1" '
        index(running, $3) > 0 {
            key = $2 SUBSEP $4
            votes[key]++
            number[$2] = $1
            size[key] = $5
            moment[key] = $6
        }
        END {
            for (path in number) {
                best = ""
                for (v = 1; v <= 3; v++) {
                    key = path SUBSEP v
                    if (!(key in votes)) continue
                    if (best == "") best = key
                    else if (votes[key] > votes[best]) best = key
                    else if (votes[key] == votes[best] && moment[key] + 0 > moment[best] + 0) best = key
                }
                split(best, parts, SUBSEP)
                printf "./%s %s %s %s\n", path, number[path], parts[2], size[best]
            }
        }' "$gen_dir/plan.txt" >"$2"
}

# Writes want.txt for the running servers: "./PATH NUMBER VERSION SIZE SHA256", sorted by path.
gen_write_expectation() {
    gen_running="$1"
    gen_choose_versions "$1" "$gen_dir/want.unsorted"
    LC_ALL=C sort -t ' ' -k 1,1 "$gen_dir/want.unsorted" >"$gen_dir/want.raw"
    [ -s "$gen_dir/want.raw" ] || fail "the expectation for the servers $1 is empty"
    : >"$gen_dir/want.txt"
    while read -r gen_name gen_index gen_version gen_size; do
        gen_want_hash="$(hash_of "$gen_dir/content/c$gen_index-v$gen_version")"
        printf '%s %s %s %s %s\n' "$gen_name" "$gen_index" "$gen_version" "$gen_size" \
            "$gen_want_hash" >>"$gen_dir/want.txt"
    done <"$gen_dir/want.raw"
}

# gen_problem NAME TEXT: one line about a path that is wrong, and below it what the plan says
# about that path, so the failure report shows holders and versions without a second look.
gen_problem() {
    {
        printf '%s: %s\n' "$1" "$2"
        awk -v name="${1#./}" -v running="$gen_running" '
            $2 == name {
                state = "stopped"
                if (index(running, $3) > 0) state = "running"
                printf "  plan: server %s (%s) holds version %s, %s bytes, time %s\n", $3, state, $4, $5, $6
            }' "$gen_dir/plan.txt"
    } >>"$gen_dir/problems.txt"
}

# One comparison of the mount with want.txt, for wait_until: the mount lists exactly the wanted
# files and each has the wanted SHA-256. It returns 1 with the details in check.txt, which the
# failure report shows. Every step returns explicitly, because wait_until runs it where set -e
# is off.
gen_matches() {
    : >"$gen_dir/problems.txt"
    list_files "$share" >"$gen_dir/got.names" 2>/dev/null
    cut -d ' ' -f 1 "$gen_dir/want.txt" >"$gen_dir/want.names"
    LC_ALL=C comm -23 "$gen_dir/want.names" "$gen_dir/got.names" >"$gen_dir/absent.names"
    LC_ALL=C comm -13 "$gen_dir/want.names" "$gen_dir/got.names" >"$gen_dir/extra.names"
    while IFS= read -r gen_name; do
        gen_problem "$gen_name" "is not listed, the plan wants it"
    done <"$gen_dir/absent.names"
    while IFS= read -r gen_name; do
        gen_problem "$gen_name" "is listed, the plan does not want it"
    done <"$gen_dir/extra.names"
    while read -r gen_name gen_index gen_version gen_size gen_want; do
        # A name that is not listed is already reported above.
        [ -f "$share/$gen_name" ] || continue
        # A file that cannot be read counts as a wrong content, with its text in the report.
        gen_got_sum="$({ sha256sum <"$share/$gen_name"; } 2>/dev/null)" || gen_got_sum="unreadable"
        gen_got="${gen_got_sum%% *}"
        [ "$gen_got" != "$gen_want" ] || continue
        gen_got_size="$(stat -c %s "$share/$gen_name" 2>/dev/null)" || gen_got_size="unknown"
        gen_problem "$gen_name" "wanted version $gen_version of path number $gen_index ($gen_size bytes, SHA-256 $gen_want), found $gen_got_size bytes, SHA-256 $gen_got"
    done <"$gen_dir/want.txt"
    if [ -s "$gen_dir/problems.txt" ]; then
        cp "$gen_dir/problems.txt" "$work/check.txt"
        return 1
    fi
    : >"$work/check.txt"
}

# gen_expect_mix RUNNING: the mount shows what the merge rules give for the running servers.
# The client may not have heard from every server at once, so it compares again for 30 s.
gen_expect_mix() {
    gen_write_expectation "$1"
    wait_until 30 "the mount shows the generated files of the servers $1" gen_matches
}

# The plan, the files, servers 1 to 3, the client, and the first check.
gen_start_mix() {
    gen_write_plan
    gen_make_files
    for gen_n in 1 2 3; do start_server "$gen_n"; done
    start_client
    gen_expect_mix 123
}

# Rule: for 60 generated paths held by different sets of servers in up to three versions, the
# mount lists exactly the paths a running server holds, and each shows the version most
# servers hold, or on a tie the newest.
gen_mix_three_servers() {
    gen_start_mix
}
scenario "a generated mix of files on three servers" gen_mix_three_servers

# Rule: the same plan after server 3 stops and the client starts again with its cache kept: the
# votes are counted among the two running servers, so paths only server 3 held are gone and the
# version of other paths may change, and no cached bytes of the old winner may be served.
gen_mix_after_server_stops() {
    gen_start_mix
    stop_client
    stop_server 3
    start_client
    gen_expect_mix 12
}
scenario "the generated mix after one server stops" gen_mix_after_server_stops
