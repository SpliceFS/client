#!/bin/sh
# Starts the container of one part of the Makefile. docker run pulls a missing image by itself,
# once and without retries, so one failed download from the registry fails the whole part. So
# the image is pulled here first: up to 3 tries, DOCKER_PULL_PAUSE seconds apart (5 by
# default), with the error of each failed try printed. Then the docker command runs as given.
# Usage: docker-run.sh IMAGE docker run ... IMAGE COMMAND (IMAGE is named twice, once to pull).
set -eu

image="$1"
shift
tries=0
until docker pull --quiet "$image" >/dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -ge 3 ]; then
        echo "docker-run: pulling $image failed $tries times" >&2
        exit 1
    fi
    sleep "${DOCKER_PULL_PAUSE:-5}"
done
exec "$@"
