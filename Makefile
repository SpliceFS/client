VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/SpliceFS/client/internal/version.version=$(VERSION)

# Image and flags for running the Linux checks from another system. The FUSE device
# and the SYS_ADMIN capability are what a container needs to mount inside itself.
LINUX_IMAGE ?= golang:1.27
LINUX_RUN := docker run --rm --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined \
	-v "$(CURDIR)":/src:ro -v splicefs-gomod:/go/pkg/mod $(LINUX_IMAGE)

# The package steps run in a plain container of the same image on every system, so a
# package built here is the package CI builds.
PACKAGE_RUN := docker run --rm -e VERSION=$(VERSION) -v "$(CURDIR)":/src:ro -v "$(CURDIR)/dist":/out \
	-v splicefs-gomod:/go/pkg/mod $(LINUX_IMAGE)

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

# Installable packages in ./dist.
package:
	mkdir -p dist
	$(PACKAGE_RUN) sh -c 'cp -r /src /tmp/w && cd /tmp/w && scripts/package.sh /out'

# Installs, removes and purges the package in a clean system.
package-test:
	$(PACKAGE_RUN) sh -c 'cd /src && scripts/package-test.sh /out'

clean:
	rm -rf bin dist coverage.out coverage.html
