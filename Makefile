VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/BlueRival/splicefs/internal/version.version=$(VERSION)

.PHONY: all build check fmt fmt-check vet test cover lint clean

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/splicefsd ./server/cmd/splicefsd
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/splicefs ./client/cmd/splicefs

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

lint:
	golangci-lint run

clean:
	rm -rf bin coverage.out coverage.html
