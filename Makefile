.PHONY: build web test vet fmt run clean sdk-bins docker tidy

BIN := dist/llmux
PKG := ./cmd/llmux

web: ## Build the embedded web app (landing/docs/dashboard) into web/dist
	npm --prefix web install --no-audit --no-fund
	npm --prefix web run build

build: ## Build the gateway binary (embeds web/dist; run `make web` first to refresh)
	@mkdir -p dist
	go build -o $(BIN) $(PKG)

test: ## Run all Go tests (race)
	go test -race ./...

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

run: build ## Build and run on :4000
	$(BIN)

clean:
	rm -rf dist sdks/python/llmux/bin sdks/node/bin

# Build the binary into each language package's bin/ dir for local dev. Real
# releases produce per-OS/arch binaries in CI and ship them in platform wheels /
# npm optionalDependencies.
sdk-bins: build
	@mkdir -p sdks/python/llmux/bin sdks/node/bin
	cp $(BIN) sdks/python/llmux/bin/llmux
	cp $(BIN) sdks/node/bin/llmux

docker: ## Build the Docker image
	docker build -t llmux:latest .

tidy:
	go mod tidy

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'
