package tools

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterTools adds all Sieve MCP tools to s using h as the handler.
// Keeping tool definitions here lets main.go stay lean and makes
// description updates reviewable in one place.
func RegisterTools(s *server.MCPServer, h *Handler) {
	// --- ctx_init ---
	s.AddTool(mcp.NewTool("ctx_init",
		mcp.WithDescription(
			"Initialize Sieve for the current session. Call this once at the start of a new conversation. "+
				"Builds the index automatically if empty, then runs SQLite query-plan optimization. "+
				"Returns ready=true when ctx_build_context is available. "+
				"If indexing is in progress, returns ready=false with indexing_progress; call again when complete.",
		),
	), h.Init)

	// --- ctx_build_context ---
	// A.2: index prerequisite note
	// A.3: build-context vs hybrid-search distinction
	s.AddTool(mcp.NewTool("ctx_build_context",
		mcp.WithDescription(
			"Full context with graph expansion and token-budget summarization. "+
				"Use when you need to read code. Slower than ctx_hybrid_search. "+
				"Requires prior indexing via ctx_index_path or auto-index on startup. "+
				"Check ctx_status indexed_files before use if results look empty.",
		),
		mcp.WithString("query", mcp.Required(), mcp.Description("What you want to understand or change.")),
	), h.BuildContext)

	// --- ctx_index_project ---
	// C.3: incremental-index description
	s.AddTool(mcp.NewTool("ctx_index_project",
		mcp.WithDescription(
			"Incrementally indexes a file or directory. "+
				"Existing index entries are preserved; only new or modified files are updated. "+
				"If path is omitted, SIEVE_ALLOWED_ROOT is used. "+
				"Only call this once at session start or when files change.",
		),
		mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT. Must be within the allowed root directory.")),
	), h.IndexProject)

	// --- ctx_drill_down ---
	// A.2: index prerequisite note
	// C.1: parameter semantics
	s.AddTool(mcp.NewTool("ctx_drill_down",
		mcp.WithDescription(
			"prefix accepts a file path (drill into directory) or a symbol ID "+
				"(expands to the containing file and its neighbors up to depth). "+
				"depth and max_tokens control graph expansion and output size respectively. "+
				"Special glob characters in prefix are treated literally. "+
				"Requires prior indexing via ctx_index_path or auto-index on startup. "+
				"Check ctx_status indexed_files before use if results look empty.",
		),
		mcp.WithString("path", mcp.Required(), mcp.Description("Directory path or file path from the branches list (e.g. 'internal/store').")),
	), h.DrillDown)

	// --- ctx_hybrid_search ---
	// A.2: index prerequisite note
	// A.3: hybrid-search vs build-context distinction
	s.AddTool(mcp.NewTool("ctx_hybrid_search",
		mcp.WithDescription(
			"Returns ranked file list only (no graph expansion). "+
				"Use when you only need file names or want to decide which files to read next. "+
				"Faster than ctx_build_context. "+
				"Requires prior indexing via ctx_index_path or auto-index on startup. "+
				"Check ctx_status indexed_files before use if results look empty.",
		),
		mcp.WithString("query", mcp.Required(), mcp.Description("Keyword or phrase.")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10).")),
	), h.HybridSearch)

	// --- ctx_trace_relation ---
	// A.2: index prerequisite note
	s.AddTool(mcp.NewTool("ctx_trace_relation",
		mcp.WithDescription(
			"Trace dependency edges from a node in the knowledge graph. "+
				"Requires prior indexing via ctx_index_path or auto-index on startup. "+
				"Check ctx_status indexed_files before use if results look empty.",
		),
		mcp.WithString("symbol", mcp.Required(), mcp.Description("Node ID (file path or symbol).")),
		mcp.WithNumber("depth", mcp.Description("BFS depth (default 2).")),
	), h.TraceRelation)

	// --- ctx_quick_exec ---
	// C.5: mark as Advanced/Internal
	s.AddTool(mcp.NewTool("ctx_quick_exec",
		mcp.WithDescription(
			"[Advanced/Internal] For verifying parser sandbox behavior. "+
				"Not needed in normal operation. "+
				"Execute a Wasm binary in a sandbox and return summarized stdout. "+
				"Only call this tool when explicitly provided with a compiled Wasm binary. "+
				"Do NOT use for images, screenshots, or any non-Wasm input.",
		),
		mcp.WithString("wasm_b64", mcp.Required(), mcp.Description("Base64-encoded compiled Wasm binary (.wasm file). Must be a valid Wasm binary, not an image or other file type.")),
		mcp.WithString("stdin", mcp.Description("Optional stdin payload.")),
	), h.QuickExec)

	// --- ctx_reset_index ---
	// A.6: destructive guard — require confirm="yes-delete-all"
	s.AddTool(mcp.NewTool("ctx_reset_index",
		mcp.WithDescription(
			"Wipe the index and re-index from scratch. "+
				"Use when the index is stale or corrupted. "+
				"This is slow — only use when ctx_index_project is insufficient. "+
				"Pass confirm=\"yes-delete-all\" to proceed.",
		),
		mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT.")),
		mcp.WithString("confirm", mcp.Description("Must be \"yes-delete-all\" to prevent accidental data loss.")),
	), h.ResetIndex)

	// --- ctx_restart_server ---
	s.AddTool(mcp.NewTool("ctx_restart_server",
		mcp.WithDescription("Terminate the Sieve server process. The MCP host will automatically re-spawn it on the next tool call. Use when the server is unresponsive or after major configuration changes. Note: the process exits cleanly; it does not hot-restart in-place."),
	), h.RestartServer)

	// --- ctx_status ---
	s.AddTool(mcp.NewTool("ctx_status",
		mcp.WithDescription("Return server status: version, uptime, node/edge counts, memory, and indexing progress."),
	), h.Status)
}
