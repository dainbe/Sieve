# Sieve Wasm Parser Guide

Sieve では、各言語の解析ロジックを `.wasm` ファイルとしてプラグイン化できます。これにより、Pure Go を維持したまま tree-sitter 等の強力な解析機能を利用可能です。

## 開発環境の準備

1.  **tree-sitter CLI**: 文法の解析に必要です。
2.  **Emscripten (emcc)**: C/C++ コードを Wasm にコンパイルするために必要です。

## Wasm ABI 仕様

Sieve の `ParserManager` は、以下の関数が Wasm モジュールからエクスポートされていることを期待します。

-   `sieve_malloc(size: i32) -> i32`: メモリ確保。
-   `sieve_free(ptr: i32)`: メモリ解放。
-   `parse(ptr: i32, len: i32) -> i32`: 
    -   入力: ソースコードのポインタと長さ。
    -   出力: 抽出されたシンボル情報（JSON文字列）へのポインタ。
    -   JSON形式: `[{"Name": "foo", "Type": "function"}, ...]`

## ビルド手順

1.  **文法の準備**:
    `tree-sitter-python` 等のディレクトリへ移動。

2.  **Cコードの生成**:
    ```bash
    tree-sitter generate
    ```

3.  **Wasmへのビルド**:
    通常、`tree-sitter build --wasm` を使用しますが、Sieve の ABI に合わせるために以下のようなラッパーを書き、`emcc` でコンパイルします。

```c
// wrapper.c
#include <stdlib.h>
#include <string.h>
#include <emscripten.h>
// tree-sitter headers...

EMSCRIPTEN_KEEPALIVE
void* sieve_malloc(size_t size) { return malloc(size); }

EMSCRIPTEN_KEEPALIVE
void sieve_free(void* ptr) { free(ptr); }

EMSCRIPTEN_KEEPALIVE
char* parse(char* code, int len) {
    // tree-sitter を使って解析し、結果を JSON 文字列として生成
    // 最後に malloc した領域に JSON を書き込み、そのポインタを返す
    return json_result;
}
```

## 配置

ビルドされた `.wasm` ファイルを、Sieve の実行ディレクトリ以下の `parsers/` ディレクトリに配置します（例: `parsers/python.wasm`）。

Sieve 起動時に `SIEVE_PARSERS_DIR` 環境変数でディレクトリを指定することも可能です。
