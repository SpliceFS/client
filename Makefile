VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/SpliceFS/client/internal/version.version=$(VERSION)

# Image and flags for running the Linux checks from another system. The FUSE device
# and the SYS_ADMIN capability are what a container needs to mount inside itself.
LINUX_IMAGE ?= golang:1.27
# Privileged, so that the tests see what a real machine and the CI runners see: /sys is
# writable, which the abort of a FUSE connection needs.
LINUX_DOCKER := docker run --rm --privileged --device /dev/fuse \
	-v "$(CURDIR)":/src:ro -v splicefs-gomod:/go/pkg/mod
LINUX_RUN := $(LINUX_DOCKER) $(LINUX_IMAGE)

# Every package format is built by its own distribution's tool inside that distribution's
# container, on every system, so a package built here is the package CI builds.
RPM_IMAGE ?= rockylinux/rockylinux:9
APK_IMAGE ?= alpine:3.22
# Empty tests this machine's own CPU type; linux/amd64 or linux/arm64 tests the other one
# by emulation (Docker Desktop provides it).
PLATFORM ?=
# Alpine versions cannot carry a commit, so builds between releases carry its date.
PACKAGE_DATE ?= $(shell git log -1 --format=%cd --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d)
PACKAGE_RUN := docker run --rm -e VERSION=$(VERSION) -e PACKAGE_DATE=$(PACKAGE_DATE) \
	-v "$(CURDIR)":/src:ro -v "$(CURDIR)/dist":/out -v splicefs-gomod:/go/pkg/mod

.PHONY: script-modes all build check check-linux fmt fmt-check vet test cover fuzz lint package-lib-test timing-test \
	package package-order-test package-test service-test repo repo-test publish-fetch publish \
	publish-check publish-test release-assets clean

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/splicefs ./cmd/splicefs

# Everything CI runs except the linter.
check: fmt-check vet cover package-lib-test timing-test script-modes tracked-files

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

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt formatted:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race ./...

# Tests with the race detector; fails below 100% statement coverage.
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

# The same checks on Linux, with real FUSE mounts, from any system that has Docker.
# The source is copied inside the container so nothing is written to the working tree.
check-linux:
	$(LINUX_RUN) sh -c 'apt-get update -qq && apt-get install -y -qq fuse3 >/dev/null && \
		cp -r /src /tmp/w && cd /tmp/w && make check'

# Proves the compatibility rule: with the same first version number, this client works with
# every released server whose second number is equal or higher, and refuses one with a lower
# second number. It builds the client of this working copy and the released servers, mounts
# for real and compares every file, so it needs Linux, FUSE and root. scripts/compat.sh says
# which servers it uses; SERVER_WORKS and SERVER_REFUSED replace that choice.
.PHONY: compat compat-linux
compat:
	scripts/compat.sh

# The same from any system that has Docker. SERVER_WORKS and SERVER_REFUSED go into the
# container only when the caller has set them. docker run takes its options before the
# image, so this line uses LINUX_DOCKER and names the image itself.
COMPAT_ENV := $(foreach v,SERVER_WORKS SERVER_REFUSED CLUSTER_ONLY CLUSTER_SEED,$(if $(filter undefined,$(origin $(v))),,-e $(v)))
compat-linux:
	$(LINUX_DOCKER) $(COMPAT_ENV) $(LINUX_IMAGE) sh -c 'apt-get update -qq && \
		apt-get install -y -qq fuse3 curl >/dev/null && \
		cp -r /src /tmp/w && cd /tmp/w && scripts/compat.sh'

# Runs the tests on FreeBSD itself: the test programs are built for FreeBSD in the Go
# container and run as root in a virtual machine of every FreeBSD release in
# packaging/freebsd/images, with the same 100% coverage gate as make check. FreeBSD cannot
# run in a container, so the machine runs under QEMU inside the Go container; this needs
# Docker, curl and xz, nothing else. FREEBSD=15.1 limits the run to one release. Where the
# machine has to be emulated (any computer but Linux on amd64) a run takes many minutes.
FREEBSD ?=
FREEBSD_ONLY ?=
.PHONY: check-freebsd
check-freebsd:
	mkdir -p dist
	$(PACKAGE_RUN) $(LINUX_IMAGE) sh -c 'cp -r /src /tmp/w && cd /tmp/w && \
		scripts/freebsd-tests.sh /out/freebsd-check'
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
# for amd64 and arm64. The program is compiled once and every format wraps that binary. The
# apk files are signed with the repository key that signing-keys.sh puts in dist/keys.
package:
	mkdir -p dist
	scripts/signing-keys.sh
	$(PACKAGE_RUN) $(LINUX_IMAGE) sh -c 'cp -r /src /tmp/w && cd /tmp/w && \
		scripts/package-binaries.sh /out && scripts/package-deb.sh /out'
	$(PACKAGE_RUN) $(RPM_IMAGE) sh -c 'cd /src && scripts/package-rpm.sh /out'
	$(PACKAGE_RUN) -v "$(CURDIR)/dist/keys":/keys:ro $(APK_IMAGE) \
		sh -c 'cd /src && scripts/package-apk.sh /out'

# FreeBSD packages in ./dist/freebsd, one per FreeBSD release and CPU type. FreeBSD cannot run
# in a container, so each release in packaging/freebsd/images runs as a virtual machine (QEMU
# inside the Go container), where pkg(8) builds the packages, installs the one for the
# machine and checks that the service runs. FREEBSD=15.1 limits the run to one release.
.PHONY: package-freebsd
package-freebsd:
	mkdir -p dist
	scripts/signing-keys.sh
	$(PACKAGE_RUN) $(LINUX_IMAGE) sh -c 'cp -r /src /tmp/w && cd /tmp/w && \
		scripts/package-binaries.sh /out'
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) LINUX_IMAGE=$(LINUX_IMAGE) \
		FREEBSD=$(FREEBSD) scripts/package-freebsd.sh dist

# The table of how a git tag becomes each package version. It needs no Docker, so it is part
# of check and runs on every system.
package-lib-test:
	scripts/package-lib-test.sh

# The timers the long test scripts print and put in the run summary. It needs no Docker, so it
# is part of check and runs on every system.
timing-test:
	scripts/timing-test.sh

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
# packages in ./dist from make package. SYSTEMS=debian:12 runs only the lines of the list that
# contain that text.
service-test:
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) PLATFORM=$(PLATFORM) scripts/service-test.sh

# Signed package repositories in ./dist/repo, made from the packages make package left in
# ./dist (it builds none). Keys come from scripts/signing-keys.sh, which is the one place that
# picks real or throwaway keys. Each format runs in its own distribution's container, with
# the keys mounted read-only.
REPO_RUN := docker run --rm -v "$(CURDIR)":/src:ro -v "$(CURDIR)/dist":/out \
	-v "$(CURDIR)/dist/keys":/keys:ro -w /src
repo:
	@ls dist/*.deb dist/*.rpm dist/alpine/*/*.apk >/dev/null 2>&1 || \
		{ echo "no packages in dist: run make package first"; exit 1; }
	scripts/signing-keys.sh
	$(REPO_RUN) $(LINUX_IMAGE) sh scripts/repo-apt.sh /out
	$(REPO_RUN) $(RPM_IMAGE) sh scripts/repo-rpm.sh /out
	$(REPO_RUN) $(APK_IMAGE) sh scripts/repo-apk.sh /out

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
