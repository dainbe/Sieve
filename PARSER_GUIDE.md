# Sieve Wasm Parser Guide

Sieve では、各言語の解析ロジックを `.wasm` ファイルとしてプラグイン化できます。
Pure Go を維持したまま tree-sitter 等の強力な解析機能を利用できます。

`SIEVE_PARSERS_DIR` に `{lang}.wasm` を置くだけで有効になります。Sieve 本体の再コンパイルは不要です。

## ダウンロード（推奨）

ビルド済みバイナリは **[dainbe/Sieve Releases](https://github.com/dainbe/Sieve/releases)** で配布しています。

```bash
bash scripts/fetch-parsers.sh        # ./parsers/ に取得
bash scripts/fetch-parsers.sh ~/parsers  # 任意のディレクトリに取得
```

スクリプトは `curl` / `wget` を自動検出し、`python.wasm` / `typescript.wasm` / `javascript.wasm` / `rust.wasm` を取得します。
既にファイルが存在する場合はスキップします。

## クイックスタート

```bash
bash scripts/fetch-parsers.sh
export SIEVE_PARSERS_DIR=./parsers
./sieve
```

---

## 言語 → ファイル名の対応

| 拡張子 | 言語キー | 必要なファイル |
|--------|---------|-------------|
| `.py` | `python` | `python.wasm` |
| `.ts` `.tsx` | `typescript` | `typescript.wasm` |
| `.js` `.jsx` | `javascript` | `javascript.wasm` |
| `.rs` | `rust` | `rust.wasm` |

Go は常に組み込みの `go/parser` AST で処理されます。`.wasm` は参照されません。

`.wasm` が存在しない言語はヒューリスティックパーサーにフォールバックします（詳細は[フォールバック動作](#フォールバック動作)参照）。

---

## Wasm ABI 仕様

すべてのパーサーバイナリは **WASI Preview 1**（`wasm32-wasi`）の呼び出し規約で以下の 3 関数をエクスポートする必要があります。

```
malloc(size: u32) -> u32
free(ptr: u32)
parse(ptr: u32, len: u32) -> u32
```

### 入力

`parse` は `ptr` / `len` で渡されたソースファイルのバイト列を受け取ります。
バッファはヌル終端されていません。`len` を使って長さを判断してください。

### 出力

`parse` は `malloc` で確保した **ヌル終端 UTF-8 JSON 文字列** へのポインタを返します。
Sieve はヌルバイトまで読み込んだ後、`free` を呼び出してポインタを解放します。

`0`（ヌルポインタ）を返すことは有効で、「シンボルなし」（`[]` と同等）として扱われます。

### JSON スキーマ

```json
[
  {
    "Name":    "function_name",
    "Type":    "function",
    "Line":    42,
    "Content": "def function_name(x, y):",
    "Calls":   ["helper", "other_fn"]
  }
]
```

| フィールド | 型 | 必須 | 説明 |
|-----------|-----|------|------|
| `Name` | string | ✅ | シンボル識別子。クロスファイルコール解決に使用 |
| `Type` | string | ✅ | 種別: `"function"` `"class"` `"method"` `"interface"` `"type"` `"variable"` `"struct"` `"enum"` `"trait"` |
| `Line` | int | — | 1-based ソース行番号。不明な場合は `0` |
| `Content` | string | ✅ | コンテキスト出力に表示されるシグネチャ行（3 行以内推奨） |
| `Calls` | []string | — | このシンボルが呼び出す関数名のリスト（非修飾名）。グラフ品質に直結 |

フィールド名は大文字小文字を区別します（`Name`/`Type` は大文字始まり）。

### クロスファイルコール解決

`Calls` に含まれる名前はインデックス時にグローバルシンボル名前表と照合されます。
同名のシンボルが複数ファイルに存在する場合はディレクトリ近接度で解決されます。
完全修飾名は不要です。短い関数名で十分です。

`Calls` が空のままだと `ctx_trace_relation` のグラフ辺が減り、クロスファイル依存追跡の精度が低下します。

---

---

> 以降はパーサーバイナリを自作・改良したい開発者向けです。
> 通常利用は「ダウンロード」セクションで完結します。
> パーサーのソースコードは [parsers/](https://github.com/dainbe/Sieve/tree/main/parsers) にあります。

## Emscripten（C/C++）でビルドする場合

### 必要なもの

- **tree-sitter CLI**: 文法の生成に使用
- **Emscripten (emcc)**: C/C++ → Wasm コンパイラ

### ラッパー実装例

```c
// wrapper.c
// Sieve は malloc / free / parse の 3 関数をエクスポートとして要求します。
// C 標準の malloc/free はそのまま使用し、-s EXPORTED_FUNCTIONS で
// エクスポートします。parse だけ EMSCRIPTEN_KEEPALIVE を付けます。
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <emscripten.h>
// tree-sitter の言語ヘッダを include

EMSCRIPTEN_KEEPALIVE
char* parse(const char* code, int len) {
    // tree-sitter で解析し、結果を JSON 文字列として構築する
    // malloc した領域に JSON を書き込み、ポインタを返す（呼び出し元が free する）
    // 例: '[]' を返す最小実装
    char* out = (char*)malloc(3);
    out[0] = '['; out[1] = ']'; out[2] = '\0';
    return out;
}
```

### ビルドコマンド例

```bash
emcc wrapper.c -o python.wasm \
  -s WASM=1 \
  -s STANDALONE_WASM=1 \
  -s EXPORTED_FUNCTIONS='["_malloc","_free","_parse"]' \
  -s EXPORTED_RUNTIME_METHODS='[]'
```

---

## Rust + wasm32-wasi でビルドする場合

Rust + tree-sitter の組み合わせが最も整備されており推奨です。

### ディレクトリ構成

```
parsers/
├── python/
│   ├── Cargo.toml
│   └── src/lib.rs
└── typescript/
    ├── Cargo.toml
    └── src/lib.rs
```

### Cargo.toml

```toml
[package]
name = "sieve-parser-python"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
tree-sitter = "0.22"
tree-sitter-python = "0.21"
serde = { version = "1", features = ["derive"] }
serde_json = "1"
```

### src/lib.rs スケルトン

```rust
use serde::Serialize;
use std::alloc::{alloc, dealloc, Layout};

#[derive(Serialize)]
struct Symbol {
    #[serde(rename = "Name")]    name:    String,
    #[serde(rename = "Type")]    kind:    String,
    #[serde(rename = "Line")]    line:    usize,
    #[serde(rename = "Content")] content: String,
    #[serde(rename = "Calls")]   calls:   Vec<String>,
}

#[no_mangle]
pub extern "C" fn malloc(size: u32) -> *mut u8 {
    let layout = Layout::from_size_align(size as usize, 1).unwrap();
    unsafe { alloc(layout) }
}

#[no_mangle]
pub extern "C" fn free(ptr: *mut u8) {
    let layout = Layout::from_size_align(1, 1).unwrap();
    unsafe { dealloc(ptr, layout) }
}

#[no_mangle]
pub extern "C" fn parse(ptr: *const u8, len: u32) -> *mut u8 {
    let src = unsafe { std::slice::from_raw_parts(ptr, len as usize) };
    let source = match std::str::from_utf8(src) {
        Ok(s) => s,
        Err(_) => return std::ptr::null_mut(),
    };

    let symbols = extract_symbols(source); // tree-sitter ロジックをここに

    let json = match serde_json::to_string(&symbols) {
        Ok(j) => j,
        Err(_) => return std::ptr::null_mut(),
    };

    // ヌル終端 JSON を返す
    let mut bytes = json.into_bytes();
    bytes.push(0);
    let layout = Layout::from_size_align(bytes.len(), 1).unwrap();
    unsafe {
        let out = alloc(layout);
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), out, bytes.len());
        out
    }
}

fn extract_symbols(_source: &str) -> Vec<Symbol> {
    // tree-sitter-python でシンボル抽出
    vec![]
}
```

### ビルド

```bash
rustup target add wasm32-wasi
cargo build --target wasm32-wasi --release
cp target/wasm32-wasi/release/sieve_parser_python.wasm /path/to/parsers/python.wasm
```

---

## パーサーのテスト

`ctx_quick_exec` MCP ツールで `.wasm` を直接実行できます。

```bash
# base64 エンコード
base64 -i python.wasm | tr -d '\n' > /tmp/b64.txt
```

```json
{
  "tool": "ctx_quick_exec",
  "wasm_b64": "<b64.txt の内容>",
  "stdin": "def hello(x): return x\n"
}
```

期待される出力: `parse` が返した JSON 配列（大きい場合は先頭で打ち切り）。

---

## 配置

```
/path/to/parsers/
├── python.wasm
├── typescript.wasm
├── javascript.wasm
└── rust.wasm
```

```bash
export SIEVE_PARSERS_DIR=/path/to/parsers
```

---

## フォールバック動作

`SIEVE_PARSERS_DIR` が未設定、または言語に対応する `.wasm` が存在しない場合、
Sieve は組み込みのヒューリスティックパーサーを使用します。

| 言語 | ヒューリスティックの能力 |
|------|----------------------|
| Python | 関数・クラス・コール（一部） |
| TypeScript / JavaScript | 関数・クラス・interface・type（コールなし） |
| Rust | fn・struct・enum・trait・impl（コール・importなし） |

Wasm パーサーが `Calls` を返す場合にのみ `ctx_trace_relation` のクロスファイル辺が張られます。

---

## デバッグ

| 症状 | 原因候補 |
|------|---------|
| `no parser found for "python"` がログに出る | `python.wasm` が `SIEVE_PARSERS_DIR` に存在しない |
| `parse: ...` エラーがログに出る | パーサーパニックまたは ABI 不一致。`ctx_quick_exec` でテスト |
| `parser returned invalid symbols JSON` | フィールド名の大文字小文字を確認（`Name`/`Type` は大文字始まり） |
| シンボルはインデックスされるがコール辺がない | JSON の `Calls` フィールドが空または欠落 |
| パーサーは動くがヒューリスティックパスが使われる | `SIEVE_PARSERS_DIR` 未設定またはパスのタイポ |

`SIEVE_DEBUG=1` を設定するとファイルごとのパーサー選択がログに出力されます。
