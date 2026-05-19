BINARY      := Sieve
MODULE      := github.com/dainbe/Sieve
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: all build install uninstall test lint bench fetch-parsers build-parsers eval bench-eval clean

all: build

build:
	go build -o $(BINARY) .

# Install Sieve globally and register it as a Claude Code MCP server.
# The sieve-mcp wrapper sets SIEVE_ALLOWED_ROOT=$PWD at launch time, so one
# registration works for all projects.
install: build
	@mkdir -p "$(INSTALL_DIR)"
	cp $(BINARY) "$(INSTALL_DIR)/sieve"
	cp scripts/sieve-mcp "$(INSTALL_DIR)/sieve-mcp"
	chmod +x "$(INSTALL_DIR)/sieve" "$(INSTALL_DIR)/sieve-mcp"
	claude mcp add sieve "$(INSTALL_DIR)/sieve-mcp" -s user
	@echo "Installed to $(INSTALL_DIR). Run 'ctx_init: {}' in any Claude Code session."

uninstall:
	claude mcp remove sieve -s user 2>/dev/null || true
	rm -f "$(INSTALL_DIR)/sieve" "$(INSTALL_DIR)/sieve-mcp"
	@echo "Uninstalled."

test:
	go test -race -timeout 120s ./...

lint:
	golangci-lint run ./...

bench:
	go test -bench=. -benchmem ./internal/store/... ./internal/context/... ./internal/indexer/...

# Download pre-built tree-sitter Wasm parsers into ./parsers/
# Set PARSERS_DIR to override the destination.
PARSERS_DIR ?= ./parsers

fetch-parsers:
	@bash scripts/fetch-parsers.sh "$(PARSERS_DIR)"

# Build tree-sitter Wasm parsers locally using Docker (avoids wasi-sdk setup).
# Output: parsers/{python,typescript,javascript,rust}.wasm
build-parsers:
	docker run --rm \
		-v "$(CURDIR)/parsers:/work" \
		-w /work \
		rust:1.85-slim \
		bash -e -c "\
apt-get update -qq && apt-get install -y -qq --no-install-recommends wget xz-utils ca-certificates && \
ARCH=\$$(uname -m | sed 's/aarch64/arm64/'); \
wget -q -P /tmp https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/wasi-sdk-24.0-\$$ARCH-linux.tar.gz && \
tar xf /tmp/wasi-sdk-24.0-\$$ARCH-linux.tar.gz -C /opt && \
export WASI_SDK=/opt/wasi-sdk-24.0-\$$ARCH-linux && \
export CC_wasm32_wasip1=\$$WASI_SDK/bin/clang && \
export CFLAGS_wasm32_wasip1=\"--sysroot=\$$WASI_SDK/share/wasi-sysroot -Wno-implicit-function-declaration\" && \
export CARGO_TARGET_WASM32_WASIP1_RUSTFLAGS=\"-C link-arg=--allow-undefined\" && \
rustup target add wasm32-wasip1 && \
cargo build --target wasm32-wasip1 --release && \
for lang in python typescript javascript rust; do \
  cp target/wasm32-wasip1/release/sieve_parser_\$$lang.wasm \$$lang.wasm; \
done"

# Run the precision/recall evaluation harness.
# Usage: make eval [EVAL_DIR=./testdata/eval]
EVAL_DIR ?= ./testdata/eval

eval:
	go test -tags eval -timeout 300s -v ./internal/eval/... -eval-dir "$(EVAL_DIR)"

# Run real-repo benchmarks: IndexProject full run + Build latency + heap.
bench-eval:
	go test -tags eval -bench=. -benchmem -run='^$$' -timeout 300s ./internal/eval/...

clean:
	rm -f $(BINARY)
