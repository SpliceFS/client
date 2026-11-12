VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/SpliceFS/client/internal/version.version=$(VERSION)

# Image and flags for running the Linux checks from another system. The FUSE device
# and the SYS_ADMIN capability are what a container needs to mount inside itself.
LINUX_IMAGE ?= golang:1.27
LINUX_RUN := docker run --rm --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined \
	-v "$(CURDIR)":/src:ro -v splicefs-gomod:/go/pkg/mod $(LINUX_IMAGE)

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

.PHONY: all build check check-linux fmt fmt-check vet test cover lint package package-test clean

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/splicefs ./cmd/splicefs

# Everything CI runs except the linter.
check: fmt-check vet cover

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

# The same checks on Linux, with real FUSE mounts, from any system that has Docker.
# The source is copied inside the container so nothing is written to the working tree.
check-linux:
	$(LINUX_RUN) sh -c 'apt-get update -qq && apt-get install -y -qq fuse3 >/dev/null && \
		cp -r /src /tmp/w && cd /tmp/w && make check'

lint:
	golangci-lint run

# Installable packages in ./dist: deb (dpkg-deb), rpm (rpmbuild) and apk (abuild), each
# for amd64 and arm64. The program is compiled once and every format wraps that binary.
package:
	mkdir -p dist
	$(PACKAGE_RUN) $(LINUX_IMAGE) sh -c 'cp -r /src /tmp/w && cd /tmp/w && \
		scripts/package-binaries.sh /out && scripts/package-deb.sh /out'
	$(PACKAGE_RUN) $(RPM_IMAGE) sh -c 'cd /src && scripts/package-rpm.sh /out'
	$(PACKAGE_RUN) $(APK_IMAGE) sh -c 'cd /src && scripts/package-apk.sh /out'

# Installs and removes each package in a clean container of every system listed in
# packaging/test-systems (CI reads the same list) and reports each system's result.
package-test:
	VERSION=$(VERSION) PACKAGE_DATE=$(PACKAGE_DATE) PLATFORM=$(PLATFORM) scripts/package-test.sh

clean:
	rm -rf bin dist coverage.out coverage.html
