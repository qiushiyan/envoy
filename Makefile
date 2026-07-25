BIN := $(HOME)/.local/bin/envoy

.PHONY: build install test vet clean

build:
	go build ./...

install:
	@mkdir -p $(dir $(BIN))
	go build -o $(BIN) ./cmd/envoy
	@echo installed $(BIN)

test:
	go test ./... -count=1

vet:
	go vet ./...

clean:
	rm -f $(BIN)
