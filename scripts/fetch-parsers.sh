#!/usr/bin/env bash
# fetch-parsers.sh — Download pre-built tree-sitter Wasm parsers for Sieve.
#
# Usage:
#   bash scripts/fetch-parsers.sh [DEST_DIR]
#
# DEST_DIR defaults to ./parsers.
#
# Each Wasm parser must expose three exports:
#   malloc(size: u32) -> u32
#   free(ptr: u32)
#   parse(ptr: u32, len: u32) -> u32   (returns pointer to null-terminated JSON)
#
# The JSON output must be an array of symbol objects:
#   [{"Name":"Foo","Type":"function","Line":10,"Content":"def Foo():"}]
#
# Parser source: https://github.com/dainbe/Sieve/tree/main/parsers
# (Build instructions in PARSER_GUIDE.md.)

set -euo pipefail

DEST="${1:-./parsers}"
REPO="https://github.com/dainbe/Sieve/releases/latest/download"
LANGS=(python typescript javascript rust)

mkdir -p "$DEST"

for lang in "${LANGS[@]}"; do
    wasm="$DEST/${lang}.wasm"
    if [ -f "$wasm" ]; then
        echo "skip: $wasm already exists"
        continue
    fi
    url="${REPO}/${lang}.wasm"
    echo "fetch: $url"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "$wasm" "$url" || { echo "warn: could not fetch ${lang}.wasm (repo may not have a release yet)"; rm -f "$wasm"; }
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$wasm" "$url" || { echo "warn: could not fetch ${lang}.wasm"; rm -f "$wasm"; }
    else
        echo "error: neither curl nor wget found"
        exit 1
    fi
done

echo "done: parsers in $DEST"
echo ""
echo "Start Sieve with SIEVE_PARSERS_DIR=$DEST to enable Wasm-based symbol extraction."
echo "Without parsers, Sieve falls back to heuristic extraction for non-Go files."
