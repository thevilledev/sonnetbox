GO ?= go
GO_TOOLCHAIN ?= go1.24.5
WASM := internal/guestblob/securejsonnet.wasm
WASM_CHECKSUM := internal/guestblob/securejsonnet.wasm.sha256
GUEST := ./cmd/securejsonnet-guest
WASM_FLAGS := -trimpath -buildvcs=false -ldflags=-buildid= -buildmode=c-shared

.PHONY: wasm wasm-check fmt-check test race fuzz no-cgo check

wasm:
	@tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOTOOLCHAIN=$(GO_TOOLCHAIN) \
		$(GO) build $(WASM_FLAGS) -o "$$tmp" $(GUEST); \
	mv "$$tmp" $(WASM); \
	shasum -a 256 $(WASM) > $(WASM_CHECKSUM)

wasm-check:
	@tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOTOOLCHAIN=$(GO_TOOLCHAIN) \
		$(GO) build $(WASM_FLAGS) -o "$$tmp" $(GUEST); \
	cmp -s "$$tmp" $(WASM) || { echo "embedded WASM is stale"; exit 1; }; \
	shasum -a 256 -c $(WASM_CHECKSUM)

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

test:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test ./...

race:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -race ./...

fuzz:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' \
		-fuzz=FuzzDecodeHostPayloads -fuzztime=10s .
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' \
		-fuzz=FuzzDecodeImportResponse -fuzztime=10s ./internal/protocol

no-cgo:
	@test -z "$$(CGO_ENABLED=1 $(GO) list -deps \
		-f '{{if .CgoFiles}}{{.ImportPath}}{{end}}' ./... | sed '/^$$/d')"

check: fmt-check test race wasm-check no-cgo
