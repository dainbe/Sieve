# GCG — Graph-Context-Go

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
SIEVE_DB_PATH=./db/sieve.db   # DB パス（デフォルト: ./sieve.db）
SIEVE_DEBUG=1            # デバッグログ有効
SIEVE_PARSERS_DIR=./parsers  # Wasm パーサーディレクトリ
```

---

## MCPクライアント連携

### Claude Code

`.claude/settings.json`（プロジェクトルート）または `~/.claude/settings.json`（グローバル）に追加：

```json
{
  "mcpServers": {
    "sieve": {
      "command": "/path/to/sieve",
      "env": {
        "SIEVE_DB_PATH": "${workspaceFolder}/.sieve/db/sieve.db"
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
      "env": { "SIEVE_DB_PATH": "${workspaceFolder}/.sieve/db/sieve.db" }
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
SIEVE_DB_PATH = ".sieve/db/sieve.db"
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
        "SIEVE_DB_PATH": ".sieve/db/sieve.db"
      }
    }
  }
}
```

---

## MCP ツール

| ツール | 説明 |
|---|---|
| `ctx_build_context` | **メインツール。** クエリに対して関連コンテキストを自律的に収集・圧縮して返す |
| `ctx_index_project` | プロジェクトをスキャン（差分のみ再インデックス） |
| `ctx_drill_down` | `ctx_build_context` の `branches` で示されたパスの詳細コンテキストを取得（Corpus2Skill の掘り下げ）|
| `ctx_hybrid_search` | FTS5 キーワード検索 |
| `ctx_trace_relation` | シンボルの依存グラフを BFS トレース |
| `ctx_quick_exec` | Wasm サンドボックスでコード実行 |
| `ctx_status` | バージョン・uptime・ノード数・メモリ使用量 |

### 典型的な使い方

```
# まずインデックスを作成
ctx_index_project: { "path": "/your/project" }

# あとは ctx_build_context だけ使えばよい
ctx_build_context: { "query": "認証処理の実装を変更したい" }
```

`ctx_build_context` は内部で FTS5 検索・グラフトレース・コンテンツ圧縮を自律的に実行し、4000トークン以内に収まる最適なコンテキストを返します。

---

## アーキテクチャ

```
AIエージェント（Claude Code / Codex / Cursor ...）
        │  MCP (stdio)
        ▼
   GCG MCP サーバー
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
