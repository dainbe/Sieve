# Sieve

[![CI](https://github.com/dainbe/Sieve/actions/workflows/ci.yml/badge.svg)](https://github.com/dainbe/Sieve/actions/workflows/ci.yml)

AIコーディングエージェントのコンテキストウィンドウを最適化する、超軽量・ローカル完結型 MCP サーバー。

コードの依存関係をグラフ化し、クエリに対して「必要最小限のコンテキスト」だけを自律的に組み立ててAIに渡します。トークン消費を削減しつつ、AIの推論精度を維持します。

## 特徴

- **Pure Go・CGO なし** — シングルバイナリで動作
- **完全ローカル** — 外部API不要、プライバシー保護
- **軽量常駐** — バックグラウンドメモリ ~8MB（dense retrieval 無効時。`SIEVE_DENSE_RETRIEVAL=0`）
- **自律的なコンテキスト圧縮** — AIがトークン予算を気にする必要なし
- **並列インデックス** — ファイルパースを `NumCPU` 並列化、DB 書き込みは単一トランザクション

---

## 最初の 5 分（クイックスタート）

```bash
# 1. クローン & ビルド
git clone https://github.com/dainbe/Sieve
cd Sieve
go build -o sieve .

# 2. プロジェクトルートを設定して起動
export SIEVE_ALLOWED_ROOT=$PWD
./sieve

# 3. セッション開始（インデックス構築 + クエリプラン最適化）
ctx_init: {}
# → ready=true になれば準備完了

# 4. 最初のクエリ
ctx_build_context: { "query": "認証処理の実装を変更したい" }
```

---

## インストール

### Claude Code で全プロジェクト共通に使う（推奨）

```bash
git clone https://github.com/dainbe/Sieve
cd Sieve
make install          # ビルド + ~/.local/bin に配置 + Claude Code にグローバル登録
```

`make install` は以下を自動で行います：

1. `sieve` バイナリをビルド
2. `~/.local/bin/sieve` と `~/.local/bin/sieve-mcp`（ラッパー）を配置
3. `claude mcp add sieve ... -s user` でグローバル登録

登録後は**どのプロジェクトでも** `ctx_init: {}` を呼ぶだけでそのプロジェクトがインデックスされます（`SIEVE_ALLOWED_ROOT` は起動時のカレントディレクトリに自動設定）。

インストール先を変更する場合：

```bash
make install INSTALL_DIR=/usr/local/bin
```

### go install

```bash
go install github.com/dainbe/Sieve@latest
```

### ソースからビルドのみ

```bash
git clone https://github.com/dainbe/Sieve
cd Sieve
go build -o sieve .
```

## 環境変数

### 必須

| 変数名 | デフォルト | 説明 |
|---|---|---|
| `SIEVE_ALLOWED_ROOT` | — | インデックス対象のプロジェクトルート。DB は `{ALLOWED_ROOT}/.sieve/sieve.db` に自動作成 |

### インデックス除外（.sieveignore）

`SIEVE_ALLOWED_ROOT/.sieveignore` にパターンを書くと、そのディレクトリ・ファイルをインデックス対象から除外できます。`.gitignore` と同じ感覚で書けます。

```
# コメント行
.claude/          # Claude Code の内部ディレクトリ
.sieve/           # Sieve DB
dist/
*.generated.ts
```

- 1行1パターン。末尾の `/` は省略可能
- ディレクトリ名の完全一致または `filepath.Match` の glob 構文をサポート
- ファイルが存在しない場合はスキップ（エラーなし）

### 推奨

| 変数名 | デフォルト | 説明 |
|---|---|---|
| `SIEVE_AUTO_INDEX` | `1` | 起動時に自動インデックスを実行（`0` で無効） |
| `SIEVE_INDEX_WORKERS` | CPU数 | ファイル並列パースのワーカー数（DB 書き込みは常に単一トランザクション）|
| `SIEVE_SCORE_THRESHOLD` | `0.25` | コンテキスト収録スコア閾値。下げると結果が増える（例: `0.1`） |
| `SIEVE_GRAPH_DEPTH` | `3` | グラフ展開の深さ。`0` でグラフ展開を無効化 |
| `SIEVE_GRAPH_SEED_TOP_K` | `30` | グラフ展開の起点として使う上位ノード数 |
| `SIEVE_DEBUG` | `0` | デバッグログ有効化 |

### 高度

| 変数名 | デフォルト | 説明 |
|---|---|---|
| `SIEVE_DENSE_RETRIEVAL` | `0` | ベクトル検索を使用（`1` で有効。モデルダウンロードが必要） |
| `SIEVE_DENSE_FRACTION` | `0.25` | dense ボーナスの重み。FTS 最高スコアに対する割合（`0` で dense ボーナス無効） |
| `SIEVE_EMBED_MODEL` | `KnightsAnalytics/all-MiniLM-L6-v2` | 使用する HuggingFace モデル名 |
| `SIEVE_MODEL_DIR` | `~/.sieve/models` | モデルキャッシュディレクトリ |
| `SIEVE_FTS_FILE_LIMIT` | `200` | FTS 検索で返すファイル数上限 |
| `SIEVE_QUERY_EXPANSION` | `0` | クエリ拡張（PPMI ベース）の近傍語数。コーパスが 200 ファイル以上のとき `3` 程度に設定すると効果が出やすい |
| `SIEVE_PPMI_MIN_COUNT` | `2` | PPMI 共起カウント最小値 |
| `SIEVE_PPMI_DISABLE` | `0` | PPMI を完全に無効化 |
| `SIEVE_PPMI_REBUILD_THRESHOLD` | `100` | PPMI 再構築をトリガーする変更ファイル数 |
| `SIEVE_MAX_FILE_BYTES` | `2097152` | インデックス対象の最大ファイルサイズ（バイト、デフォルト 2MB） |
| `SIEVE_PARSERS_DIR` | `./parsers` | Wasm パーサーディレクトリ |
| `SIEVE_EVAL_DUMP` | `0` | eval 時のコンテキスト出力をダンプ |

---

## MCPクライアント連携

Sieve は **MCP サーバー 1 インスタンス = 1 プロジェクト = 1 DB** として運用します。DB は `SIEVE_ALLOWED_ROOT/.sieve/sieve.db` に自動作成されるため、プロジェクトごとに `SIEVE_ALLOWED_ROOT` を設定するだけで分離されます。

### Claude Code

`.claude/settings.json`（プロジェクトルート）または `~/.claude/settings.json`（グローバル）に追加：

```json
{
  "mcpServers": {
    "sieve": {
      "command": "/path/to/sieve",
      "env": {
        "SIEVE_ALLOWED_ROOT": "${workspaceFolder}"
      }
    }
  }
}
```

CLIで追加する場合：

```bash
claude mcp add sieve /path/to/sieve
```

#### PreToolUse hook でコンテキストを自動注入（任意）

AIが明示的にツールを呼ばなくても、ファイル編集前に自動でコンテキストを差し込む設定：

```json
{
  "mcpServers": {
    "sieve": {
      "command": "/path/to/sieve",
      "env": {
        "SIEVE_ALLOWED_ROOT": "${workspaceFolder}"
      }
    }
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          {
            "type": "command",
            "command": "sh -c 'FILE=$(echo \"$CLAUDE_TOOL_INPUT\" | jq -r \".file_path // empty\"); [ -n \"$FILE\" ] && sieve-query \"$FILE\" || true'"
          }
        ]
      }
    ]
  }
}
```

> `sieve-query` は Sieve の MCP ツール `ctx_build_context` を呼ぶ薄いラッパースクリプトです。hook はオプションです。MCP ツールとして直接呼ぶだけで十分です。

---

### Codex CLI

`~/.codex/config.toml`（グローバル）または `.codex/config.toml`（プロジェクト）に追加：

```toml
[mcp_servers.sieve]
command = "/path/to/sieve"
args = []

[mcp_servers.sieve.env]
SIEVE_ALLOWED_ROOT = "/path/to/project"
```

CLIで追加する場合：

```bash
codex mcp add sieve -- /path/to/sieve
```

確認：

```bash
codex mcp list
```

---

### その他の MCP クライアント（Cursor 等）

stdio 形式の MCP サーバーとして登録します。Cursor の場合は `.cursor/mcp.json`：

```json
{
  "mcpServers": {
    "sieve": {
      "command": "/path/to/sieve",
      "args": [],
      "env": {
        "SIEVE_ALLOWED_ROOT": "/path/to/project"
      }
    }
  }
}
```

---

## MCP ツール

| ツール | 説明 | レスポンス |
|---|---|---|
| `ctx_init` | **セッション開始時に呼ぶ。** インデックスが空なら自動構築し、SQLite クエリプラン最適化を実行。`ready=true` になれば他のツールが使える | ready・indexed_files・newly_indexed・optimized |
| `ctx_build_context` | **メインツール。** クエリに対して関連コンテキストを自律的に収集・圧縮して返す | nodes・branches・suggested_next |
| `ctx_index_project` | プロジェクトをスキャン（差分のみ再インデックス） | ctx_status 形式 |
| `ctx_reset_index` | インデックスを全削除して再構築。インデックスが壊れた場合や初回以外のコード大幅変更時に使用 | ctx_status 形式 |
| `ctx_restart_server` | サーバープロセスを終了（MCP ホストが次回ツール呼び出し時に自動で再 spawn）。サーバーが応答しないときや設定変更後に使用 | メッセージ |
| `ctx_drill_down` | `ctx_build_context` の `branches` で示されたパスの詳細コンテキストを取得 | nodes・branches・suggested_next |
| `ctx_hybrid_search` | FTS5 キーワード検索（サマリーのみ）。ファイル名一覧を素早く絞り込みたいときに使用 | nodes |
| `ctx_trace_relation` | シンボルの依存グラフを BFS トレース | edges |
| `ctx_status` | バージョン・uptime・ノード数・メモリ使用量・DB パス・インデックス進捗 | status |

**Advanced / Internal:**

| ツール | 説明 | レスポンス |
|---|---|---|
| `ctx_quick_exec` | [Internal] Wasm パーサーのサンドボックス動作確認用。通常運用では不要 | stdout |

### ツール選択の決定木

```
セッション開始時（必ず最初に）              → ctx_init
コードを読みたい・graph 展開が必要         → ctx_build_context
ファイル名一覧が欲しい・高速に候補を絞りたい → ctx_hybrid_search
```

### 典型的な使い方

```
# 1. セッション開始時に必ず呼ぶ（インデックス構築 + 最適化）
ctx_init: {}

# 2. クエリ（これだけで事足りる場合がほとんど）
ctx_build_context: { "query": "認証処理の実装を変更したい" }

# 3. コンテキストが不十分なら suggested_next を掘り下げる
ctx_drill_down: { "path": "backend/app/api" }

# ファイルが増えたとき（差分のみ更新）
ctx_index_project: { "path": "/your/project" }

# インデックスが壊れたとき（全再構築）
ctx_reset_index: { "confirm": "yes-delete-all" }
```

`ctx_build_context` は内部で FTS5 検索・グラフトレース・コンテンツ圧縮を自律的に実行し、4000トークン以内に収まる最適なコンテキストを返します。

---

## よくある質問

### ctx_init と ctx_index_project の違いは？

| | `ctx_init` | `ctx_index_project` |
|---|---|---|
| 呼ぶタイミング | **セッション開始時（毎回）** | ファイルを追加・変更したとき |
| インデックスが空 | 自動構築する | 構築する |
| インデックスが存在 | スキップ（最適化のみ） | 差分更新 |
| SQLite 最適化 | 実行する | しない |

セッション開始時は常に `ctx_init` を呼んでください。ファイルを追加・編集した後は `ctx_index_project` で差分更新します。

---

### インデックスの再読み込みはどうする？

ファイルを追加・変更した場合は `ctx_index_project` を再呼び出しするだけで差分のみ更新されます（全消しはしません）。

```
ctx_index_project: { "path": "/your/project" }
```

ファイル監視（watch）機能は現時点では未対応です。IDE の on-save hook や Git の post-commit hook で `ctx_index_project` を呼ぶ運用を推奨します。インデックスが壊れた場合は `ctx_reset_index: { "confirm": "yes-delete-all" }` で全再構築してください。

---

## 結果が悪いときのデバッグ手順

1. **`ctx_init` を呼ぶ** — インデックスが空なら自動構築し、`ready=true` を返す。`ctx_status` の `indexed_files` が 0 のままなら `SIEVE_ALLOWED_ROOT` の設定を確認。

2. **スコア閾値を下げる** — デフォルト `0.25` を下げると結果が増える（ノイズも増える）：
   ```bash
   SIEVE_SCORE_THRESHOLD=0.1 ./sieve
   ```

3. **グラフ展開を切る** — グラフのノイズを疑うとき：
   ```bash
   SIEVE_GRAPH_DEPTH=0 ./sieve
   ```

4. **FTS+graph のみにする** — ベクトル検索を無効化：
   ```bash
   SIEVE_DENSE_RETRIEVAL=0 ./sieve
   ```

5. **現在の設定を確認**：
   ```bash
   ./sieve --help
   ```

6. **並列インデックスを無効化** — インデックス結果が不安定なとき：
   ```bash
   SIEVE_INDEX_WORKERS=1 ./sieve
   ```

7. **特定ディレクトリをインデックスから除外** — `.sieveignore` をプロジェクトルートに作成：
   ```
   .claude/
   node_modules/
   dist/
   ```

---

## eval ハーネスの実行

```bash
# precision/recall を 30 ケースで計測（FTS + グラフのみ、dense 無効）
go test -tags eval -timeout 300s -v -run TestEval_SieveRepo ./internal/eval/...

# dense retrieval を有効にして計測（モデルダウンロードが必要）
SIEVE_DENSE_RETRIEVAL=1 go test -tags eval -timeout 300s -v -run TestEval_SieveRepo ./internal/eval/...

# コンテキスト出力をダンプして確認
SIEVE_EVAL_DUMP=1 go test -tags eval -v -run TestEval_SieveRepo ./internal/eval/...
```

メトリクス: P@5, R@5, MRR, nDCG@5, InformationDensity, EfficiencyScore

**最新ベースライン（commit `bd2b2c0`、n=30 ケース）:**

| モード | P@5 | R@5 | MRR | nDCG@5 | 遅延 |
|---|---|---|---|---|---|
| FTS+グラフ（dense 無効） | 0.288 | 0.983 | 0.983 | 0.961 | ~8ms |
| FTS+グラフ+dense | 0.293 | 1.000 | 0.983 | 0.962 | ~90ms |

> dense ON の遅延増はクエリ埋め込み計算コスト（~80ms）によるもの。品質は同等以上。

---

## アーキテクチャ

```
AIエージェント（Claude Code / Codex / Cursor ...）
        │  MCP (stdio)
        ▼
   Sieve MCP サーバー
        │
   ┌────┴────────────────┐
   │  ctx_init           │  ← セッション開始時
   │  - 空なら自動インデックス
   │  - PRAGMA optimize  │
   └────┬────────────────┘
        │
   ┌────┴────────────────┐
   │  ctx_build_context  │  ← クエリ時
   │  - FTS5 検索        │
   │  - グラフトレース    │
   │  - コンテンツ圧縮    │
   └────┬────────────────┘
        │
   ┌────┴────────────────┐
   │  Indexer            │  ← インデックス時
   │  - 並列パース (N)    │  SIEVE_INDEX_WORKERS
   │  - 単一 writer batch │  SQLite 単一ライター保証
   └────┬────────────────┘
        │
   SQLite (FTS5 + 知識グラフ)
   wazero サンドボックス
   Go AST パーサー
```

## 開発

```bash
# ビルド・テスト
go build ./...        # または make build
go test -race ./...   # または make test

# Lint
golangci-lint run ./...  # または make lint

# ベンチマーク
make bench

# Wasm パーサーのダウンロード（オプション：精度向上）
make fetch-parsers  # PARSERS_DIR=./parsers に .wasm を配置

# 精度評価（eval ハーネス）
make eval           # precision/recall を testdata/eval/cases/ のケースで計測
```

## フェーズ進捗

| Phase | 状態 | 内容 |
|---|---|---|
| 1 | ✅ | MCP サーバー基盤、stdio 通信 |
| 2 | ✅ | SQLite + FTS5 + 知識グラフ、RWMutex、WAL checkpoint |
| 3 | ✅ | wazero サンドボックス、Go AST シンボル抽出、Wasm パーサー |
| 4 | ✅ | インクリメンタルインデックス、FTS サニタイズ、削除同期 |
| 5 | ✅ | ctx_build_context、コンテキスト自律圧縮、slog 構造化ログ |
| 6 | ✅ | Python / TypeScript / JavaScript / Rust heuristic シンボル抽出 |
| 7 | ✅ | PageIndex 思想の取り込み（branches summary・suggested_next・Insufficient フラグ） |
| 8 | ✅ | ctx_reset_index・ctx_restart_server 追加、DB 自動配置（SIEVE_ALLOWED_ROOT 必須化）|
| 9 | ✅ | 依存更新（mcp-go v0.54・wazero v1.11・sqlite v1.50）、DB schema バージョニング、Go AST calls エッジ、AST ベース圧縮、eval ハーネス |
| 10 | ✅ | dense semantic retrieval（hugot ONNX embedding、VectorIndex、eval integration）|
| 13 | ✅ | PPMI クエリ拡張インフラ（term_neighbors テーブル、ExpandQuery、SIEVE_QUERY_EXPANSION）|
| 14 | ✅ | FTS content augmentation（camelCase / snake_case トークン化で検索精度向上）|
| 15 | ✅ | デフォルト graphDepth 2→3、グラフ展開強化 |
| 16 | ✅ | core-concept metrics ハーネス（P@5・R@5・MRR・nDCG@5・density 測定基盤）|
| 17 | ✅ | compression×density balance metrics、score threshold filter（情報密度向上）|
| — | ✅ | 監査修正スプリント（クラッシュ・データロス・eval ハーネス安定化、audit 28 件）|
| — | ✅ | usability sprint（起動時自動インデックス、進捗ステータス、`--help/--version/--config`、destructive ガード、PPMI メモリ最適化）|
| — | ✅ | critic 修正（dense default 整合、PPMI prune trigger 修正、`--config` required 表示、restart_server 文書化）|
| — | ✅ | 並列インデクサ（ファイルパース N-worker 並列化＋単一 writer batch、SIEVE_INDEX_WORKERS）|
| — | ✅ | ctx_init ツール（セッション開始時の自動インデックス＋SQLite クエリプラン最適化）|
| — | ✅ | Wasm パーサー配布（GitHub Releases + ctx_init 自動取得、Rust wasm32-wasip1 CI）|
| — | ✅ | dense 正規化バグ修正（VectorIndex クエリ L2 正規化）+ eval n=15→30 拡張 |
| — | ✅ | builder.go スコアリングパイプライン分離（6 懸念→名前付きメソッド）、env.Int/Float 共通化（env パッケージ）|
| — | ✅ | JS/Rust Wasm パーサー実装（tree-sitter 0.21 `language()` API 対応）、`make build-parsers` Docker ビルドターゲット追加 |
| — | ✅ | `.sieveignore` サポート（ユーザー定義の除外パターン、glob 対応）|