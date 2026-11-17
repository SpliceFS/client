# The version this commit is, or will be, released as: the file VERSION, which the release bot
# sets on every pull request. Git history is not read for it, because CI checks out one commit
# without tags. -dirty marks uncommitted changes to tracked files; no git checkout is clean.
# This is the one place that works it out: the scripts receive it as VERSION.
VERSION ?= v$(shell cat VERSION)$(if $(shell git status --porcelain --untracked-files=no 2>/dev/null),-dirty)
LDFLAGS := -s -w -X github.com/SpliceFS/client/internal/version.version=$(VERSION)

# The tools are in our images (ghcr.io/splicefs) and nothing of ours is: the source is mounted
# into the image when a part runs. The Go image also runs the checks, the build and the Debian
# packages; every other package format is built by its own distribution's tools in its own
# image, so a package built here is the package CI builds.
LINUX_IMAGE ?= ghcr.io/splicefs/go:v1
RPM_IMAGE ?= ghcr.io/splicefs/rpm-build:v1
APK_IMAGE ?= ghcr.io/splicefs/apk-build:v1
# Empty tests this machine's own CPU type; linux/amd64 or linux/arm64 tests the other one
# by emulation (Docker Desktop provides it).
PLATFORM ?=
# Alpine versions cannot carry a commit, so a build with uncommitted changes carries the date
# of its commit.
PACKAGE_DATE ?= $(shell git log -1 --format=%cd --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d)
# What the package scripts need to name their files. They read these as environment
# variables; env sets them in an image that has no make.
PACKAGE_VARS = VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE)

# One runner per image: docker run with the source at /src, as the caller's own user, so that
# every file a part writes belongs to the caller and not to root. The Go caches are folders
# under CACHE, mounted where the images keep them; a named Docker volume is made by root,
# which a user other than root cannot write to. A target that needs more options sets
# RUN_OPTS; a target that has to run as root (RUN_USER) also keeps its own caches
# (GO_CACHE), so that no file in the caller's caches belongs to root.
CACHE ?= $(CURDIR)/.cache
CACHE_DIRS := $(CACHE)/gomod $(CACHE)/gobuild
ROOT_CACHE := $(CACHE)/root
ROOT_CACHE_DIRS := $(ROOT_CACHE)/gomod $(ROOT_CACHE)/gobuild
RUN_OPTS :=
RUN_USER = $(shell id -u):$(shell id -g)
GO_CACHE = $(CACHE)
DOCKER_RUN = docker run --rm --user "$(RUN_USER)" $(RUN_OPTS) -v "$(CURDIR)":/src -w /src
# The cache paths are named here as well as in the go image, so that the run works with any
# Go image and any uid: HOME, GOPATH and GOCACHE all point at the mounted host folders.
LINUX_RUN = $(DOCKER_RUN) -v "$(GO_CACHE)/gomod":/tmp/go/pkg/mod -v "$(GO_CACHE)/gobuild":/tmp/go-cache \
	-e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/go-cache \
	-e GOLANGCI_LINT_CACHE=/tmp/go-cache/golangci-lint $(LINUX_IMAGE)
RPM_RUN = $(DOCKER_RUN) $(RPM_IMAGE)
APK_RUN = $(DOCKER_RUN) $(APK_IMAGE)
# The signing keys, read only, for the steps that sign or check with them.
KEYS_MOUNT := -v "$(CURDIR)/dist/keys":/keys:ro

# Docker makes a folder that does not exist yet as root, so the caller makes them first.
$(CACHE_DIRS) $(ROOT_CACHE_DIRS):
	mkdir -p $@

.PHONY: script-modes all build check check-in-images fmt fmt-check vet cover fuzz lint package-lib-test \
	freebsd-lib-test version-test timing-test server-lib-test part-test build-all-test package \
	package-order-test package-test service-test repo repo-test publish-fetch publish publish-check \
	publish-test release-assets clean

all: check build

# Every program in cmd/ goes to bin/ under its own name.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

# The parts of a build. Each is a make target that runs its inner target (the same name with
# -here, the real work) in the image and prints how long it took (scripts/part.sh). A part
# runs alone on any machine that has Docker, and in CI, with the same command; parts that do
# not need each other can run at the same time. A -here target also runs on a machine that
# has the tools.
.PHONY: static static-checks static-here lint-go unit unit-here race race-here build-all build-all-here \
	integration packages package-deb package-rpm package-apk keys

# gofmt, go vet, golangci-lint and ShellCheck, and the two checks that read the git index.
# Those run on the host and not in the image: a git worktree keeps its index outside the
# folder that is mounted.
static: | $(CACHE_DIRS)
	scripts/part.sh static $(MAKE) --no-print-directory static-checks

static-checks: script-modes tracked-files
	$(LINUX_RUN) make static-here

static-here: fmt-check vet tidy-check lint-shell lint-go

# go.mod and go.sum must be what go mod tidy writes, so that a dependency that is no longer
# used, or one that is used and not listed, is found in the pull request. -diff changes no file.
.PHONY: tidy-check
tidy-check:
	go mod tidy -diff

# golangci-lint for each system the code is built for, because the files of a system are only
# checked when that system is the target.
lint-go:
	@set -e; for os in linux darwin freebsd; do \
		echo "golangci-lint GOOS=$$os"; GOOS=$$os golangci-lint run; \
	done

# The tests of mount/fuse mount for real, and nothing else of the client needs more than an
# ordinary user. As root the client mounts with mount(2); any other user needs a fusermount3
# that finds the user in the passwd file and may mount, which an image does not give an
# arbitrary uid. So the parts that run those tests (unit, race, integration) run as root, with
# the FUSE device, privileged as the CI runners are (/sys is writable, which the abort of a FUSE
# connection needs), and keep their own caches under CACHE/root.
MOUNT_PARTS := unit race integration
$(MOUNT_PARTS): RUN_USER = 0
$(MOUNT_PARTS): GO_CACHE = $(ROOT_CACHE)
$(MOUNT_PARTS): RUN_OPTS = --privileged --device /dev/fuse

# The tests with the 100% coverage gate, and the tests of the scripts that need no Docker.
unit: | $(ROOT_CACHE_DIRS)
	scripts/part.sh unit $(LINUX_RUN) make unit-here

unit-here: cover package-lib-test freebsd-lib-test version-test timing-test server-lib-test part-test \
	build-all-test

# The same tests with the race detector, which makes them several times slower.
race: | $(ROOT_CACHE_DIRS)
	scripts/part.sh race $(LINUX_RUN) make race-here

race-here:
	go test -race ./...

# Every program, for every system and CPU type, into ./dist/bin. The packages, the release and
# the other parts read from there; nothing is compiled twice.
build-all: | $(CACHE_DIRS)
	scripts/part.sh build-all $(LINUX_RUN) make $(PACKAGE_VARS) build-all-here

build-all-here:
	LDFLAGS='$(LDFLAGS)' scripts/build-all.sh dist/bin

# This client against the released servers, in a container that may mount (FUSE) as root;
# SERVER_WORKS, SERVER_REFUSED, CLUSTER_ONLY and CLUSTER_SEED reach the container when set.
integration: RUN_OPTS += -e SERVER_WORKS -e SERVER_REFUSED -e CLUSTER_ONLY -e CLUSTER_SEED
integration: | $(ROOT_CACHE_DIRS)
	scripts/part.sh integration $(LINUX_RUN) make compat

# The packages of every format, from the programs in ./dist/bin (make build-all). Each format
# is a target of its own.
packages: package-deb package-rpm package-apk

package-deb: | $(CACHE_DIRS)
	scripts/part.sh package-deb $(LINUX_RUN) env $(PACKAGE_VARS) sh scripts/package-deb.sh /src/dist

package-rpm:
	scripts/part.sh package-rpm $(RPM_RUN) env $(PACKAGE_VARS) sh scripts/package-rpm.sh /src/dist

package-apk: RUN_OPTS = $(KEYS_MOUNT)
package-apk: keys
	scripts/part.sh package-apk $(APK_RUN) env $(PACKAGE_VARS) sh scripts/package-apk.sh /src/dist

# The image the Go parts run in. CI keys its Go caches on it (.github/actions/go-cache), so
# that a new image starts with empty caches.
.PHONY: go-image
go-image:
	@echo $(LINUX_IMAGE)

# Decides which keys everything is signed with and puts them in ./dist/keys.
keys:
	LINUX_IMAGE=$(LINUX_IMAGE) scripts/signing-keys.sh

# What a machine that has every tool runs: the three parts, natively.
check: script-modes tracked-files static-here unit-here race-here

# The same on any system that has Docker, in the images.
check-in-images: static unit race

# A script that lost its executable bit fails only where it is run directly, which for the
# publishing script is the release job. This finds it in every check. Libraries (-lib.sh, and
# timing.sh) are read by other scripts and never run, and so are the scenario files of the
# cluster test.
script-modes:
	@bad="$$(git ls-files -s -- 'scripts/*.sh' 'packaging/*.sh' | awk '$$1 != "100755" && $$4 !~ /-lib\.sh$$/ && $$4 != "scripts/timing.sh" && $$4 !~ /^scripts\/cluster-scenarios\// {print $$4}')"; \
	if [ -n "$$bad" ]; then echo "not executable in git:"; echo "$$bad"; exit 1; fi

# Nothing that must stay private may be tracked: no compiled program (it carries the folder
# names of the machine it was built on), no file over 1 MiB, no name of a private machine,
# account or place. The ignore rules help; this check is what stops a mistake.
.PHONY: tracked-files
tracked-files:
	scripts/tracked-files.sh

# Not gofmt on the folder itself: that would also format the module cache in ./.cache. Hidden
# folders hold no source of this project.
GO_FILES = find . -name '*.go' -not -path './.*'

fmt:
	$(GO_FILES) | xargs gofmt -w

fmt-check:
	@out="$$($(GO_FILES) | xargs gofmt -l)"; if [ -n "$$out" ]; then echo "not gofmt formatted:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

# Tests with coverage; fails below 100% statement coverage. The race detector has a part of
# its own (race).
cover:
	scripts/coverage.sh coverage.out

# Go runs one fuzz target per invocation, so each target is found with go test -list and run
# by itself. Only directories that contain a Fuzz function are visited: the mount packages
# need root and FUSE (or the NFS client) to test, and none of their code is fuzzed, so this
# target needs neither. A new Fuzz function is picked up without editing this file.
FUZZTIME ?= 10s
fuzz:
	@set -e; for dir in $$(grep -rl --include='*_test.go' '^func Fuzz' . | xargs -n1 dirname | sort -u); do \
		for target in $$(go test -list '^Fuzz' $$dir | grep '^Fuzz'); do \
			echo "fuzz $$dir $$target ($(FUZZTIME))"; \
			go test -run '^$$' -fuzz "^$$target\$$" -fuzztime $(FUZZTIME) $$dir; \
		done; \
	done

# Proves the compatibility rule: with the same first version number, this client works with
# every released server whose second number is equal or higher, and refuses one with a lower
# second number. It builds the client of this working copy and the released servers, mounts
# for real and compares every file, so it needs Linux, FUSE and root. scripts/compat.sh says
# which servers it uses; SERVER_WORKS and SERVER_REFUSED replace that choice. make integration
# runs it in a container that has FUSE.
.PHONY: compat
compat:
	scripts/compat.sh

# Runs the tests on FreeBSD itself: the test programs are built for FreeBSD in the Go
# container and run as root in a virtual machine of every FreeBSD release in
# packaging/freebsd/images, with the same 100% coverage gate as make check. FreeBSD cannot
# run in a container, so the machine runs under QEMU inside the Go container; this needs
# Docker, curl and xz, nothing else. FREEBSD=15.1 limits the run to one release. Where the
# machine has to be emulated (any computer but Linux on amd64) a run takes many minutes.
FREEBSD ?=
FREEBSD_ONLY ?=
.PHONY: check-freebsd
check-freebsd: | $(CACHE_DIRS)
	$(LINUX_RUN) sh scripts/freebsd-tests.sh dist/freebsd-check
	LINUX_IMAGE=$(LINUX_IMAGE) FREEBSD=$(FREEBSD) FREEBSD_ONLY=$(FREEBSD_ONLY) scripts/check-freebsd.sh dist

# ShellCheck runs with every optional check on (see .shellcheckrc). The globs pick up a new
# script in scripts/ without editing this file; the maintainer and Alpine scripts have no .sh
# extension, so they are named. The OpenRC script (not a shell ShellCheck knows), the rpm spec
# and the FreeBSD manifest template are not shell files and are left out.
lint: lint-shell
	golangci-lint run

# The shell part of lint by itself, for the CI job that has no Go tools.
.PHONY: lint-shell
lint-shell:
	shellcheck scripts/*.sh scripts/cluster-scenarios/*.sh packaging/*.sh packaging/*/*.sh \
		packaging/deb/postinst packaging/deb/prerm packaging/deb/postrm \
		packaging/alpine/*.post-install packaging/alpine/*.post-upgrade \
		packaging/alpine/*.pre-deinstall \
		packaging/freebsd/splicefs.rc packaging/freebsd/user-data.in

# Installable packages in ./dist: deb (dpkg-deb), rpm (rpmbuild) and apk (abuild), each
# for amd64 and arm64. The program is compiled once (build-all) and every format wraps that
# binary. The apk files are signed with the repository key that signing-keys.sh puts in
# dist/keys.
package: build-all
	$(MAKE) --no-print-directory packages

# FreeBSD packages in ./dist/freebsd, one per FreeBSD release and CPU type. FreeBSD cannot run
# in a container, so each release in packaging/freebsd/images runs as a virtual machine (QEMU
# inside the Go container), where pkg(8) builds the packages, installs the one for the
# machine and checks that the service runs. FREEBSD=15.1 limits the run to one release.
.PHONY: package-freebsd
package-freebsd: build-all keys
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) LINUX_IMAGE=$(LINUX_IMAGE) \
		FREEBSD=$(FREEBSD) scripts/package-freebsd.sh dist

# Where the version comes from: the file VERSION, also in a checkout of one commit without
# tags, which is what CI has. It needs no Docker, so it is part of check.
version-test:
	scripts/version-test.sh

# The table of how a version becomes each package version. It needs no Docker, so it is part
# of check and runs on every system.
package-lib-test:
	scripts/package-lib-test.sh

freebsd-lib-test:
	scripts/freebsd-lib-test.sh

# The helper that prints how long a part took, and the script that compiles every program.
# Neither needs Docker nor a compiler, so both are part of check.
part-test:
	scripts/part-test.sh

build-all-test:
	scripts/build-all-test.sh

# The timers the long test scripts print and put in the run summary. It needs no Docker, so it
# is part of check and runs on every system.
timing-test:
	scripts/timing-test.sh

# How the test scripts ask a server for a password hash, with older and newer servers. It needs
# no Docker, so it is part of check and runs on every system.
server-lib-test:
	scripts/server-lib-test.sh

# Checks in a Debian, a Rocky Linux and an Alpine container that those versions sort the way
# a release needs (a pre-release before its release) and that apk accepts them. It is a
# prerequisite of package-test, not of check, because it needs Docker; package-test is what CI
# and the release job already run before anything is published.
package-order-test:
	scripts/package-order-test.sh

# Installs and removes each package in a clean container of every system listed in
# packaging/test-systems (CI reads the same list) and reports each system's result.
package-test: package-order-test
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) PLATFORM=$(PLATFORM) scripts/package-test.sh

# Proves the service, not only the files: in a container per system in packaging/test-systems
# that runs that system's own service manager, installs the package and checks that the
# service starts by itself, runs as the user the unit names, answers the status command,
# restarts, comes back after a reboot and stops when the package is removed. It needs the
# packages in ./dist from make package. ONLY=debian:12 or ONLY=deb runs only that image or
# that format.
service-test:
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) PLATFORM=$(PLATFORM) scripts/service-test.sh

# Signed package repositories in ./dist/repo, made from the packages make package left in
# ./dist (it builds none). Keys come from scripts/signing-keys.sh, which is the one place that
# picks real or throwaway keys. Each format runs in the image of its own distribution's tools,
# with the keys mounted read-only.
repo: RUN_OPTS = $(KEYS_MOUNT)
repo: keys | $(CACHE_DIRS)
	@ls dist/*.deb dist/*.rpm dist/alpine/*/*.apk >/dev/null 2>&1 || \
		{ echo "no packages in dist: run make package first"; exit 1; }
	$(LINUX_RUN) sh scripts/repo-apt.sh /src/dist
	$(RPM_RUN) sh scripts/repo-rpm.sh /src/dist
	$(APK_RUN) sh scripts/repo-apk.sh /src/dist

# Installs from the repository on every system in packaging/test-systems with signature
# checks on, and checks that a damaged repository is refused.
repo-test:
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) PLATFORM=$(PLATFORM) scripts/package-test.sh repo

# Publishing to packages.splicefs.org/client. UPLOAD_KEY_FILE and UPLOAD_KNOWN_HOSTS_FILE
# (see scripts/publish.sh) come from the environment, so no secret is ever in this file.
# Downloads what is published into dist/repo first, so the repositories built next keep every
# older version.
publish-fetch:
	scripts/publish.sh fetch

# Uploads dist/repo. It refuses an incomplete repository and any upload that would delete a
# published package.
publish:
	scripts/publish.sh send

# Lists the remote root and changes nothing: proves that the key and host key work.
publish-check:
	scripts/publish.sh check

# Proves publish.sh and release-assets.sh against local folders and throwaway keys, with no
# server. Needs dist/repo and dist/keys from make package repo, and changes neither.
publish-test:
	scripts/publish-test.sh

# The files of this version for the GitHub release, in dist/release: the packages, SHA256SUMS
# and its signature. SIGNING_KEY_FILE is the OpenPGP signing key; the default is the key
# signing-keys.sh put in dist/keys.
SIGNING_KEY_FILE ?= $(CURDIR)/dist/keys/splicefs-openpgp-signing-subkey.asc
release-assets:
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) SIGNING_KEY_FILE=$(SIGNING_KEY_FILE) \
		scripts/release-assets.sh

clean:
	rm -rf bin dist coverage.out coverage.html
