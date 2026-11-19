#!/bin/sh
# Test of scripts/docker-run.sh with a stand-in docker that fails the first pulls. Runs on the
# host, no Docker and no network (make docker-run-test, part of make check). The pause between
# tries is set to 0, so the test does not wait.
set -eu

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat > "$tmp/bin/docker" <<'STANDIN'
#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
case "$1" in
    pull)
        pulls=$(grep -c '^pull' "$FAKE_DOCKER_LOG")
        if [ "$pulls" -le "$FAKE_FAIL_PULLS" ]; then
            echo "docker: failed to copy: httpReadSeeker: failed open: failed to do request" >&2
            exit 1
        fi
        echo "sha256:0000"
        ;;
esac
STANDIN
chmod 755 "$tmp/bin/docker"
failed=0
image=ghcr.io/splicefs/go:v1

# $1 name, $2 pulls that fail, $3 want status, $4 want pulls, $5 want run (yes or no)
check() {
    log="$tmp/$1.log"
    : > "$log"
    status=0
    out="$(PATH="$tmp/bin:$PATH" FAKE_DOCKER_LOG="$log" FAKE_FAIL_PULLS="$2" DOCKER_PULL_PAUSE=0 \
        scripts/docker-run.sh "$image" docker run --rm "$image" make unit-here 2>&1)" || status=$?
    pulls=$(grep -c '^pull' "$log" || true)
    ran=no
    if grep -q '^run ' "$log"; then ran=yes; fi
    if [ "$status" -eq "$3" ] && [ "$pulls" -eq "$4" ] && [ "$ran" = "$5" ]; then
        printf 'ok   %-28s status %s, pulls %s, run %s\n' "$1" "$status" "$pulls" "$ran"
    else
        printf 'FAIL %-28s want status %s pulls %s run %s; got status %s pulls %s run %s\n' \
            "$1" "$3" "$4" "$5" "$status" "$pulls" "$ran"
        failed=$((failed + 1))
    fi
    if ! printf '%s' "$out" | grep -q 'failed to copy'; then
        echo "FAIL $1 does not show the pull error"
        failed=$((failed + 1))
    fi
}

check pull-fails-once 1 0 2 yes
check pull-always-fails 99 1 3 no

if [ "$failed" -ne 0 ]; then
    echo "$failed case(s) failed"
    exit 1
fi
echo "all docker-run cases passed"
