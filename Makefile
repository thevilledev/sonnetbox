GO ?= go
GOLANGCI_LINT ?= golangci-lint
CARGO ?= cargo
GO_VERSION := $(strip $(shell sed -n '1p' .go-version))
GO_TOOLCHAIN ?= go$(GO_VERSION)
TEST_TIMEOUT ?= 5m
RACE_TIMEOUT ?= 3m
RACE_TESTS ?= ^(TestConcurrencyLimitHonorsContext|TestFreshInstancesAndConcurrentEvaluation)$$
FUZZ_TIME ?= 10s
BENCH_TIME ?= 5x
BENCH_TIMEOUT ?= 20m
COVERAGE_MIN ?= 70
GO_JSONNET_VERSION = $(strip $(shell $(GO) list -m -f '{{.Version}}' github.com/google/go-jsonnet))
BUILD_DIR := build
# The conformance oracle is the upstream C++ suite at one pinned commit. Keep
# this in step with the ref the CI job checks out.
JSONNET_SUITE_REF ?= 5aec27e03a61dae06461becb95089b15fe217233
JSONNET_SUITE_CACHE := $(BUILD_DIR)/conformance/jsonnet-$(JSONNET_SUITE_REF)
JSONNET_SUITE_DIR ?= $(JSONNET_SUITE_CACHE)/test_suite
COVERAGE_PROFILE := $(BUILD_DIR)/coverage.out
WASM_DIR := guest
WASM := $(WASM_DIR)/sonnetbox.wasm
WASM_CHECKSUM := $(WASM_DIR)/sonnetbox.wasm.sha256
GUEST := ./cmd/sonnetbox-guest
WASM_FLAGS := -trimpath -buildvcs=false -ldflags=-buildid= -buildmode=c-shared
IMAGE ?= ghcr.io/thevilledev/sonnetbox
VERSION ?= dev
DOCKER ?= docker

.PHONY: bench cli conformance conformance-suite docker fmt fmt-check lint \
	mod-check test coverage race fuzz fuzz-smoke no-cgo wasm wasm-check \
	rust-fmt-check rust-lint rust-test rust-check check ci

bench:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' -bench=. \
		-benchtime=$(BENCH_TIME) -timeout=$(BENCH_TIMEOUT) .

cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) build -trimpath \
		-o $(BUILD_DIR)/sonnetbox ./cmd/sonnetbox

# conformance-suite fetches the pinned upstream suite so the comparison runs
# from a bare checkout. A caller-supplied JSONNET_SUITE_DIR is used as is.
conformance-suite:
	@set -eu; \
	if [ -d "$(JSONNET_SUITE_DIR)" ]; then exit 0; fi; \
	if [ "$(JSONNET_SUITE_DIR)" != "$(JSONNET_SUITE_CACHE)/test_suite" ]; then \
		echo "JSONNET_SUITE_DIR $(JSONNET_SUITE_DIR) does not exist" >&2; \
		exit 1; \
	fi; \
	rm -rf "$(JSONNET_SUITE_CACHE)"; \
	mkdir -p "$(JSONNET_SUITE_CACHE)"; \
	git -C "$(JSONNET_SUITE_CACHE)" init -q; \
	git -C "$(JSONNET_SUITE_CACHE)" remote add origin \
		https://github.com/google/jsonnet.git; \
	git -C "$(JSONNET_SUITE_CACHE)" fetch -q --depth 1 origin $(JSONNET_SUITE_REF); \
	git -C "$(JSONNET_SUITE_CACHE)" checkout -q FETCH_HEAD

conformance: cli conformance-suite
	@mkdir -p $(BUILD_DIR)/conformance/bin
	GOBIN=$(abspath $(BUILD_DIR)/conformance/bin) GOTOOLCHAIN=local \
		$(GO) install github.com/google/go-jsonnet/cmd/jsonnet@$(GO_JSONNET_VERSION)
	JSONNET_SUITE_DIR=$(abspath $(JSONNET_SUITE_DIR)) \
		SONNETBOX_BIN=$(abspath $(BUILD_DIR)/sonnetbox) \
		GO_JSONNET_BIN=$(abspath $(BUILD_DIR)/conformance/bin/jsonnet) \
		bash test/conformance/jsonnet-suite.sh

docker:
	$(DOCKER) build \
		--build-arg GO_VERSION=$(GO_VERSION) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		.

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
		-run='$(RACE_TESTS)' -timeout=$(RACE_TIMEOUT) .

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
	cd $(WASM_DIR) && shasum -a 256 sonnetbox.wasm > sonnetbox.wasm.sha256

wasm-check:
	@set -eu; \
	tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm GOTOOLCHAIN=$(GO_TOOLCHAIN) \
		$(GO) build $(WASM_FLAGS) -o "$$tmp" $(GUEST); \
	cmp -s "$$tmp" $(WASM) || { echo "embedded WASM is stale"; exit 1; }; \
	cd $(WASM_DIR) && shasum -a 256 -c sonnetbox.wasm.sha256

no-cgo:
	CGO_ENABLED=0 GOTOOLCHAIN=local $(GO) test -run='^$$' -count=1 \
		-timeout=$(TEST_TIMEOUT) ./...

rust-fmt-check:
	$(CARGO) fmt --all -- --check

rust-lint:
	$(CARGO) clippy --workspace --all-targets --locked -- -D warnings

rust-test:
	$(CARGO) test --workspace --locked

rust-check: rust-fmt-check rust-lint rust-test

check: fmt-check mod-check lint coverage no-cgo wasm-check rust-check

ci: check race fuzz-smoke
