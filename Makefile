.PHONY: build site-docs site-chrome site-chrome-selftest test test-embed test-ffi ffi ffi-bench cover cover-html record smoke vet fmt fmt-check verify-selftest gates run clean sdk-bins sdk-test docker tidy help

BIN := dist/llmux
PKG := ./cmd/llmux

site-docs: ## Regenerate site/docs from the canonical docs/ tree (CI fails if stale)
	go run ./site/gen

# The admin console (web/ui.html) is a single hand-written file embedded via
# go:embed — no npm, no build step, nothing to regenerate. `go build` alone
# picks up any edit to it.
build: ## Build the gateway binary (embeds web/ui.html)
	@mkdir -p dist
	go build -o $(BIN) $(PKG)

test: ## Run all Go tests (race)
	go test -race ./...

# embedtest is a separate module (github.com/vul-os/llmux-embedtest), so the
# `test` target above never reaches it. It runs through the gate rather than
# plain `go test` because `go test` exits 0 when nothing ran — see the
# embeddability job in .github/workflows/ci.yml, which uses the same command.
test-embed: ## Run the embeddability guards (separate module, gated)
	./scripts/go-test-gate.sh --dir embedtest --min 15 \
		--require TestInternalWallRefusesAnInternalImport \
		--require TestModulePathSitsOutsideTheLibraryPrefix \
		--require TestLibraryChatThroughPublicAPI \
		--require TestConstructionStartsNoGoroutines \
		--require TestConstructionMakesZeroRoundTrips \
		--require TestConstructionAdoptsNoUnnamedProviderKey \
		--require TestGatewayDependsOnNeitherServerNorUI \
		--require TestLibraryOnlyHostLinksNoUIBytes \
		--require TestNoUIBuildDropsTheEmbedAndIsSmaller \
		--require TestBothBuildStatesServeTheDocumentedUIResponse \
		-- -count=1 ./...

# ffi/ is the C-ABI shared library and, like embedtest, a separate Go module —
# so `test` above never reaches it either. It also needs cgo, which is why it is
# not folded into `test`.
test-ffi: ## Run the C-ABI unit tests (separate module, gated) AND the C smoke test
	./scripts/go-test-gate.sh --dir ffi --min 14 \
		--require TestABIVersionMatchesThePackageVersion \
		--require TestFFIUsesOnlyThePublicAPI \
		--require TestUnknownHandleIsACleanError \
		--require TestCloseIsIdempotentAndHandlesAreNotReused \
		--require TestChatCallReturnsTheHTTPAPIsJSON \
		--require TestStreamDeliversChunksThatReassembleTheAnswer \
		--require TestStreamAbortFromTheCallbackIsNotAnError \
		--require TestTwoHandlesAreIndependent \
		-- -count=1 ./...
	./scripts/ffi-ctest.sh

ffi: ## Build the C-ABI shared library for this host into dist/ffi
	./scripts/build-ffi.sh

ffi-bench: ffi ## Measure in-process (C ABI) vs loopback HTTP latency
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
	case "$$os" in windows) lib=llmux.dll ;; darwin) lib=libllmux.dylib ;; *) lib=libllmux.so ;; esac; \
	cd ffi && go run ./bench -lib "../dist/ffi/$${os}_$${arch}/$${lib}"

cover: ## Coverage summary (set LLMUX_TEST_POSTGRES/LLMUX_TEST_REDIS to include integration)
	go test -cover ./...

cover-html: ## Generate an HTML coverage report at coverage.html
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html"

record: ## Record golden fixtures from REAL provider APIs (needs keys); LLMUX_RECORD=1
	LLMUX_RECORD=1 go test -count=1 -run Conformance ./core/conformance/...

smoke: ## Run the live smoke suite against real providers (needs keys); LLMUX_LIVE=1
	LLMUX_LIVE=1 go test -count=1 -run Live ./...

vet: ## Static analysis
	go vet ./...

fmt: ## Format
	go fmt ./...

fmt-check: ## gofmt gate over EVERY Go directory (the check CI runs)
	./scripts/gofmt-check.sh

verify-selftest: ## Prove scripts/verify.sh still REFUSES every broken-release shape
	bash scripts/verify.sh --selftest

site-chrome: ## Assert the site's ratified chrome: no docs footer, pinned rail, zero outbound origins, every fence language vendored
	node scripts/check-docs-chrome.mjs

site-chrome-selftest: ## Prove site-chrome still FAILS when each of its assertions is broken
	node scripts/check-docs-chrome.test.mjs

# The artifact-level gates, in the order CI runs them. `test` is separate
# because it needs no Node; these are the ones that catch a green build
# shipping a wrong artifact — or shipping a right artifact nobody can check.
gates: fmt-check verify-selftest test-embed test-ffi site-chrome site-chrome-selftest ## Run the artifact gates (gofmt scope + release-verifier matrix + embeddability + C ABI + site chrome)

run: build ## Build and run on :4000
	$(BIN)

clean:
	rm -rf dist sdks/python/llmux/bin sdks/node/bin sdks/ruby/bin sdks/php/bin \
		sdks/rust/bin sdks/java/bin sdks/dotnet/bin sdks/elixir/priv/bin

# Build the binary into each language package's bin/ dir for local dev. Real
# releases produce per-OS/arch binaries in CI and ship them in platform wheels /
# npm optionalDependencies.
sdk-bins: build
	@mkdir -p sdks/python/llmux/bin sdks/node/bin sdks/ruby/bin sdks/php/bin \
		sdks/rust/bin sdks/java/bin sdks/dotnet/bin sdks/elixir/priv/bin
	cp $(BIN) sdks/python/llmux/bin/llmux
	cp $(BIN) sdks/node/bin/llmux
	cp $(BIN) sdks/ruby/bin/llmux
	cp $(BIN) sdks/php/bin/llmux
	cp $(BIN) sdks/rust/bin/llmux
	cp $(BIN) sdks/java/bin/llmux
	cp $(BIN) sdks/dotnet/bin/llmux
	cp $(BIN) sdks/elixir/priv/bin/llmux

SDK_BIN := /tmp/llmux-sdk-test-bin

sdk-test: ## Run every available SDK test suite (skips missing toolchains)
	@echo ">> building gateway binary for SDK integration tests"
	@GOFLAGS=-mod=mod GOPROXY=off go build -o $(SDK_BIN) ./cmd/llmux
	@echo ">> go"
	@go test ./sdks/go/... || exit 1
	@if command -v python3 >/dev/null 2>&1; then \
		echo ">> python"; \
		( cd sdks/python && LLMUX_BINARY=$(SDK_BIN) python3 -m unittest discover -s tests ) || exit 1; \
	else echo ">> python: skipped (python3 not found)"; fi
	@if command -v node >/dev/null 2>&1; then \
		echo ">> node"; \
		( cd sdks/node && LLMUX_BINARY=$(SDK_BIN) node --test ) || exit 1; \
	else echo ">> node: skipped (node not found)"; fi
	@if command -v ruby >/dev/null 2>&1; then \
		echo ">> ruby"; \
		( cd sdks/ruby && LLMUX_BINARY=$(SDK_BIN) ruby -Ilib -Itest test/test_llmux.rb ) || exit 1; \
	else echo ">> ruby: skipped (ruby not found)"; fi
	@if command -v cargo >/dev/null 2>&1; then \
		echo ">> rust"; \
		( cd sdks/rust && LLMUX_BINARY=$(SDK_BIN) cargo test --offline ) || exit 1; \
	else echo ">> rust: skipped (cargo not found)"; fi
	@if command -v mvn >/dev/null 2>&1; then \
		echo ">> java (maven/junit)"; \
		( cd sdks/java && LLMUX_BINARY=$(SDK_BIN) mvn -q test ) || exit 1; \
	elif command -v javac >/dev/null 2>&1 && javac -version >/dev/null 2>&1; then \
		echo ">> java (plain javac/java check)"; \
		( cd sdks/java && LLMUX_BINARY=$(SDK_BIN) sh run-java-check.sh ) || exit 1; \
	else echo ">> java: skipped (no jdk)"; fi
	@if command -v composer >/dev/null 2>&1 && command -v php >/dev/null 2>&1; then \
		echo ">> php"; \
		( cd sdks/php && composer install --quiet && LLMUX_BINARY_REAL=$(SDK_BIN) vendor/bin/phpunit ) || exit 1; \
	else echo ">> php: skipped (php/composer not found)"; fi
	@if command -v dotnet >/dev/null 2>&1; then \
		echo ">> dotnet"; \
		( cd sdks/dotnet && LLMUX_BINARY_REAL=$(SDK_BIN) dotnet test tests/Llmux.Tests.csproj ) || exit 1; \
	else echo ">> dotnet: skipped (dotnet not found)"; fi
	@if command -v mix >/dev/null 2>&1; then \
		echo ">> elixir"; \
		( cd sdks/elixir && LLMUX_BINARY_REAL=$(SDK_BIN) mix test ) || exit 1; \
	else echo ">> elixir: skipped (mix not found)"; fi
	@echo ">> sdk-test done"

docker: ## Build the Docker image
	docker build -t llmux:latest .

tidy:
	go mod tidy

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'
