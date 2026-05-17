package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
	"github.com/dainbe/Sieve/internal/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const version = "0.1.0"

func main() {
	// --- Structured logging ---
	logLevel := slog.LevelInfo
	if os.Getenv("SIEVE_DEBUG") == "1" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})))

	// --- Allowed root (required) ---
	allowedRoot := os.Getenv("SIEVE_ALLOWED_ROOT")
	if allowedRoot == "" {
		slog.Error("SIEVE_ALLOWED_ROOT is required; set it to the project root directory")
		os.Exit(1)
	}

	// --- Store: DB is always placed inside the project root ---
	// {SIEVE_ALLOWED_ROOT}/.sieve/sieve.db
	dbPath := filepath.Join(allowedRoot, ".sieve", "sieve.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		slog.Error("create db directory failed", "err", err)
		os.Exit(1)
	}
	db, err := store.New(dbPath)
	if err != nil {
		slog.Error("store init failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// --- Parser manager (optional) ---
	var pm *indexer.ParserManager
	if parsersDir := os.Getenv("SIEVE_PARSERS_DIR"); parsersDir != "" {
		pm, err = indexer.NewParserManager(parsersDir)
		if err != nil {
			slog.Warn("parser manager init failed", "err", err)
		} else {
			defer func() { _ = pm.Close() }()
		}
	}

	// --- MCP server ---
	s := server.NewMCPServer("sieve", version)
	h := tools.NewHandler(db, pm, allowedRoot, version)

	s.AddTool(mcp.NewTool("ctx_build_context",
		mcp.WithDescription("Build optimal context for a query. Collects, scores, and compresses relevant code into the minimum tokens needed for accurate reasoning."),
		mcp.WithString("query", mcp.Required(), mcp.Description("What you want to understand or change.")),
	), h.BuildContext)

	s.AddTool(mcp.NewTool("ctx_index_project",
		mcp.WithDescription("Scan a project directory and build the knowledge graph. If path is omitted, SIEVE_ALLOWED_ROOT is used. Only changed files are re-processed. Only call this once at session start or when files change."),
		mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT. Must be within the allowed root directory.")),
	), h.IndexProject)

	s.AddTool(mcp.NewTool("ctx_drill_down",
		mcp.WithDescription("Drill into a directory branch for fuller content. Use the 'branches' field from ctx_build_context to find explorable paths."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Directory path or file path from the branches list (e.g. 'internal/store').")),
	), h.DrillDown)

	s.AddTool(mcp.NewTool("ctx_hybrid_search",
		mcp.WithDescription("Keyword search over indexed content. Returns summaries only. Use ctx_build_context for full context."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Keyword or phrase.")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10).")),
	), h.HybridSearch)

	s.AddTool(mcp.NewTool("ctx_trace_relation",
		mcp.WithDescription("Trace dependency edges from a node in the knowledge graph."),
		mcp.WithString("symbol", mcp.Required(), mcp.Description("Node ID (file path or symbol).")),
		mcp.WithNumber("depth", mcp.Description("BFS depth (default 2).")),
	), h.TraceRelation)

	s.AddTool(mcp.NewTool("ctx_quick_exec",
		mcp.WithDescription("Execute a Wasm binary in a sandbox and return summarized stdout. Only call this tool when explicitly provided with a compiled Wasm binary. Do NOT use for images, screenshots, or any non-Wasm input."),
		mcp.WithString("wasm_b64", mcp.Required(), mcp.Description("Base64-encoded compiled Wasm binary (.wasm file). Must be a valid Wasm binary, not an image or other file type.")),
		mcp.WithString("stdin", mcp.Description("Optional stdin payload.")),
	), h.QuickExec)

	s.AddTool(mcp.NewTool("ctx_reset_index",
		mcp.WithDescription("Wipe the index and re-index from scratch. Use when the index is stale or corrupted. This is slow — only use when ctx_index_project is insufficient."),
		mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT.")),
	), h.ResetIndex)

	s.AddTool(mcp.NewTool("ctx_restart_server",
		mcp.WithDescription("Restart the Sieve server process. Use when the server is unresponsive or after major configuration changes. The MCP host will restart the process automatically."),
	), h.RestartServer)

	s.AddTool(mcp.NewTool("ctx_status",
		mcp.WithDescription("Return server status: version, uptime, node/edge counts, memory."),
	), h.Status)

	slog.Info("Sieve MCP server starting", "version", version, "db", dbPath, "allowed_root", allowedRoot)
	if err := server.ServeStdio(s); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}