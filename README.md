# Sieve

AIコーディングエージェントのコンテキストウィンドウを最適化する、超軽量・ローカル完結型 MCP サーバー。

コードの依存関係をグラフ化し、クエリに対して「必要最小限のコンテキスト」だけを自律的に組み立ててAIに渡します。トークン消費を削減しつつ、AIの推論精度を維持します。

## 特徴

- **Pure Go・CGO なし** — シングルバイナリで動作
- **完全ローカル** — 外部API不要、プライバシー保護
- **軽量常駐** — バックグラウンドメモリ ~8MB
- **自律的なコンテキスト圧縮** — AIがトークン予算を気にする必要なし

## インストール

```bash
go install github.com/dainbe/Sieve@latest
```

またはソースからビルド：

```bash
git clone https://github.com/dainbe/Sieve
cd Sieve
go build -o sieve .
```

## クイックスタート

```bash
# サーバー起動
./sieve

# 環境変数
SIEVE_ALLOWED_ROOT=/path/to/project    # プロジェクトルート（必須）。DBは {ALLOWED_ROOT}/.sieve/sieve.db に自動作成される
SIEVE_DEBUG=1                          # デバッグログ有効
SIEVE_PARSERS_DIR=./parsers            # Wasm パーサーディレクトリ
```

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
| `ctx_build_context` | **メインツール。** クエリに対して関連コンテキストを自律的に収集・圧縮して返す | nodes・branches・suggested_next |
| `ctx_index_project` | プロジェクトをスキャン（差分のみ再インデックス） | ctx_status 形式 |
| `ctx_reset_index` | インデックスを全削除して再構築。インデックスが壊れた場合や初回以外のコード大幅変更時に使用 | ctx_status 形式 |
| `ctx_restart_server` | サーバープロセスを再起動。MCP ホストが自動的に再起動する | メッセージ |
| `ctx_drill_down` | `ctx_build_context` の `branches` で示されたパスの詳細コンテキストを取得 | nodes・branches・suggested_next |
| `ctx_hybrid_search` | FTS5 キーワード検索（サマリーのみ） | nodes |
| `ctx_trace_relation` | シンボルの依存グラフを BFS トレース | edges |
| `ctx_quick_exec` | Wasm サンドボックスでコード実行 | stdout |
| `ctx_status` | バージョン・uptime・ノード数・メモリ使用量・DB パス | status |

### 典型的な使い方

```
# まずインデックスを作成（初回・またはファイルが増えたとき）
ctx_index_project: { "path": "/your/project" }

# インデックスをリセットして再構築（インデックスが壊れたとき）
ctx_reset_index: { "path": "/your/project" }

# あとは ctx_build_context だけ使えばよい
ctx_build_context: { "query": "認証処理の実装を変更したい" }

# コンテキストが不十分なら suggested_next を掘り下げる
ctx_drill_down: { "path": "backend/app/api" }
```

`ctx_build_context` は内部で FTS5 検索・グラフトレース・コンテンツ圧縮を自律的に実行し、4000トークン以内に収まる最適なコンテキストを返します。

---

## アーキテクチャ

```
AIエージェント（Claude Code / Codex / Cursor ...）
        │  MCP (stdio)
        ▼
   Sieve MCP サーバー
        │
   ┌────┴────────────────┐
   │  ctx_build_context  │  ← コア機能
   │  - FTS5 検索        │
   │  - グラフトレース    │
   │  - コンテンツ圧縮    │
   └────┬────────────────┘
        │
   SQLite (FTS5 + 知識グラフ)
   wazero サンドボックス
   Go AST パーサー
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