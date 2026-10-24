PREFIX ?= $(HOME)/.local
BIN := $(PREFIX)/bin/envoy

.PHONY: build install test vet fix check golden clean

build:
	go build ./...

# Callers run the binary by path, and every dispatch re-executes it to detach
# its turn: build beside it and rename into place, so nothing ever execs a
# half-written file.
install:
	@mkdir -p "$(PREFIX)/bin"
	@tmp=$$(mktemp "$(BIN).XXXXXX"); trap 'rm -f "$$tmp"' EXIT; \
	go build -o "$$tmp" ./cmd/envoy && chmod 755 "$$tmp" && mv -f "$$tmp" "$(BIN)"
	@echo installed $(BIN)

# -count=1: tests/ builds and drives the binary itself, which the test cache
# cannot see.
test:
	go test -race -count=1 ./...

vet:
	go vet ./...

# Go 1.27 modernizers must leave nothing to rewrite.
fix:
	@out=$$(go fix -diff ./...); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

check: vet fix test
	@test -z "$$(gofmt -l cmd internal tests)"

# Rewrite the golden help pages after an intended change; review the diff.
golden:
	CLIKIT_UPDATE=1 go test ./cmd/envoy -run 'TestHelpPages$$' -count=1

clean:
	rm -f $(BIN)
