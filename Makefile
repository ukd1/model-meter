GO      ?= go
BINARY  ?= model-meter

.PHONY: all build test race vet vuln fmt check clean

all: check build

# The SQLite driver needs cgo and a working C toolchain.
build:
	CGO_ENABLED=1 $(GO) build -o $(BINARY) .

test:
	$(GO) test ./...

# Accounting, sessions and the live notifier all share state across goroutines.
race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# Reported rather than rewritten, so a local run and CI agree on the result.
fmt:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

# Built with the current toolchain rather than a preinstalled binary, so the
# standard library is analysed against the Go version this project targets.
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

check: vet fmt test

clean:
	rm -f $(BINARY)