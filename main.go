package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
	"github.com/dainbe/Sieve/internal/tools"
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

	// --- Store (auto-create parent directory) ---
	dbPath := envOr("SIEVE_DB_PATH", "./db/sieve.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		slog.Error("create db directory failed", "err", err)
		os.Exit(1)
	}
	db, err := store.New(dbPath)
	if err != nil {
		slog.Error("store init failed", "err", err)
		os.Exit(1)
	}

	// --- Allowed root for path traversal protection ---
	allowedRoot := os.Getenv("SIEVE_ALLOWED_ROOT")

	// --- Parser manager (optional) ---
	var pm *indexer.ParserManager
	if parsersDir := os.Getenv("SIEVE_PARSERS_DIR"); parsersDir != "" {
		pm, err = indexer.NewParserManager(parsersDir)
		if err != nil {
			slog.Warn("parser manager init failed", "err", err)
		}
	}

	// --- MCP server ---
	s := server.NewMCPServer("sieve", version, server.WithToolCapabilities(false))
	h := tools.NewHandler(db, pm, allowedRoot, version)

	s.AddTool(mcp.NewTool("ctx_build_context",
		mcp.WithDescription("Build optimal context for a query. Collects, scores, and compresses relevant code into the minimum tokens needed for accurate reasoning."),
		mcp.WithString("query", mcp.Required(), mcp.Description("What you want to understand or change.")),
	), h.BuildContext)

	s.AddTool(mcp.NewTool("ctx_index_project",
		mcp.WithDescription("Scan a project directory and build the knowledge graph. Only changed files are re-processed."),
		mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to the project root.")),
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
		mcp.WithDescription("Execute a Wasm binary in a sandbox and return summarized stdout."),
		mcp.WithString("wasm_b64", mcp.Required(), mcp.Description("Base64-encoded Wasm binary.")),
		mcp.WithString("stdin", mcp.Description("Optional stdin payload.")),
	), h.QuickExec)

	s.AddTool(mcp.NewTool("ctx_status",
		mcp.WithDescription("Return server status: version, uptime, node/edge counts, memory."),
	), h.Status)

	slog.Info("GCG MCP server starting", "version", version, "db", dbPath)
	if err := server.ServeStdio(s); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}

	slog.Info("shutdown: closing store")
	_ = db.Close()
	if pm != nil {
		_ = pm.Close()
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
