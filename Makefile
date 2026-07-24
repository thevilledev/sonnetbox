GO ?= go
GOLANGCI_LINT ?= golangci-lint
GO_VERSION := $(strip $(shell sed -n '1p' .go-version))
GO_TOOLCHAIN ?= go$(GO_VERSION)
TEST_TIMEOUT ?= 5m
RACE_TIMEOUT ?= 10m
FUZZ_TIME ?= 10s
COVERAGE_MIN ?= 70
BUILD_DIR := build
COVERAGE_PROFILE := $(BUILD_DIR)/coverage.out
WASM := internal/guestblob/sonnetbox.wasm
WASM_CHECKSUM := internal/guestblob/sonnetbox.wasm.sha256
GUEST := ./cmd/sonnetbox-guest
WASM_FLAGS := -trimpath -buildvcs=false -ldflags=-buildid= -buildmode=c-shared

.PHONY: cli fmt fmt-check lint mod-check test coverage race fuzz fuzz-smoke \
	no-cgo wasm wasm-check check ci

cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) build -trimpath \
		-o $(BUILD_DIR)/sonnetbox ./cmd/sonnetbox

fmt:
	$(GOLANGCI_LINT) fmt

fmt-check:
	$(GOLANGCI_LINT) fmt --diff

lint:
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run

mod-check:
	GOTOOLCHAIN=local $(GO) mod tidy -diff
	GOTOOLCHAIN=local $(GO) mod verify

test:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -count=1 \
		-timeout=$(TEST_TIMEOUT) ./...

coverage:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -count=1 \
		-timeout=$(TEST_TIMEOUT) -covermode=atomic \
		-coverprofile=$(COVERAGE_PROFILE) ./...
	$(GO) tool cover -func=$(COVERAGE_PROFILE)
	@coverage="$$( $(GO) tool cover -func=$(COVERAGE_PROFILE) | \
		awk '/^total:/ {gsub(/%/, "", $$3); print $$3}' )"; \
	awk -v actual="$$coverage" -v minimum="$(COVERAGE_MIN)" \
		'BEGIN { \
			if (actual + 0 < minimum + 0) { \
				printf "coverage %.1f%% is below %.1f%%\n", actual, minimum; \
				exit 1; \
			} \
			printf "coverage %.1f%% meets %.1f%% minimum\n", actual, minimum; \
		}'

race:
	CGO_ENABLED=1 GOTOOLCHAIN=local $(GO) test -race -count=1 \
		-timeout=$(RACE_TIMEOUT) ./...

fuzz:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' \
		-timeout=$(TEST_TIMEOUT) -fuzz=FuzzDecodeHostPayloads \
		-fuzztime=$(FUZZ_TIME) .
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' \
		-timeout=$(TEST_TIMEOUT) -fuzz=FuzzDecodeImportResponse \
		-fuzztime=$(FUZZ_TIME) ./internal/protocol
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' \
		-timeout=$(TEST_TIMEOUT) -fuzz=FuzzPack \
		-fuzztime=$(FUZZ_TIME) ./internal/protocol

fuzz-smoke:
	$(MAKE) fuzz FUZZ_TIME=10000x

wasm:
	@set -eu; \
	tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOTOOLCHAIN=$(GO_TOOLCHAIN) \
		$(GO) build $(WASM_FLAGS) -o "$$tmp" $(GUEST); \
	mv "$$tmp" $(WASM); \
	shasum -a 256 $(WASM) > $(WASM_CHECKSUM)

wasm-check:
	@set -eu; \
	tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOTOOLCHAIN=$(GO_TOOLCHAIN) \
		$(GO) build $(WASM_FLAGS) -o "$$tmp" $(GUEST); \
	cmp -s "$$tmp" $(WASM) || { echo "embedded WASM is stale"; exit 1; }; \
	shasum -a 256 -c $(WASM_CHECKSUM)

no-cgo:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' -count=1 \
		-timeout=$(TEST_TIMEOUT) ./...

check: fmt-check mod-check lint coverage no-cgo wasm-check

ci: check race fuzz-smoke
