#!/bin/sh
# Writes the two files that let an ordinary user mount with fusermount3 inside an image, into
# FOLDER: passwd and fuse.conf. The part that mounts as an ordinary user (make fuse) mounts
# them over /etc/passwd and /etc/fuse.conf of the image.
#
# fusermount3 asks the passwd file for the name of the user and refuses with "could not
# determine username" when there is none, and an image has no entry for the uid of whoever
# runs it. fuse.conf holds what an administrator allows ordinary users: with user_allow_other
# they may mount with allow_other, which the tests of mount/fuse check.
#
# usage: fuse-user.sh FOLDER [UID GID]    UID and GID default to the caller's own.
set -eu

if [ "$#" -ne 1 ] && [ "$#" -ne 3 ]; then
    echo "usage: fuse-user.sh FOLDER [UID GID]" >&2
    exit 2
fi
folder="$1"
user_id="${2:-$(id -u)}"
group_id="${3:-$(id -g)}"

mkdir -p "$folder"
printf 'root:x:0:0:root:/root:/bin/sh\nuser:x:%s:%s:user:/tmp:/bin/sh\n' "$user_id" "$group_id" >"$folder/passwd"
printf 'user_allow_other\n' >"$folder/fuse.conf"
