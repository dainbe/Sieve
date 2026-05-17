package tools

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Tools returns the full list of MCP tools this handler exposes.
// main.go registers them via s.AddTools(h.Tools()...).
func (h *Handler) Tools() []server.ServerTool {
	return []server.ServerTool{
		{
			Tool: mcp.NewTool("ctx_build_context",
				mcp.WithDescription("Build optimal context for a query. Collects, scores, and compresses relevant code into the minimum tokens needed for accurate reasoning."),
				mcp.WithString("query", mcp.Required(), mcp.Description("What you want to understand or change.")),
			),
			Handler: h.BuildContext,
		},
		{
			Tool: mcp.NewTool("ctx_index_project",
				mcp.WithDescription("Scan a project directory and build the knowledge graph. If path is omitted, SIEVE_ALLOWED_ROOT is used. Only changed files are re-processed. Only call this once at session start or when files change."),
				mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT. Must be within the allowed root directory.")),
			),
			Handler: h.IndexProject,
		},
		{
			Tool: mcp.NewTool("ctx_drill_down",
				mcp.WithDescription("Drill into a directory branch for fuller content. Use the 'branches' field from ctx_build_context to find explorable paths."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Directory path or file path from the branches list (e.g. 'internal/store').")),
			),
			Handler: h.DrillDown,
		},
		{
			Tool: mcp.NewTool("ctx_hybrid_search",
				mcp.WithDescription("Keyword search over indexed content. Returns summaries only. Use ctx_build_context for full context."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Keyword or phrase.")),
				mcp.WithNumber("limit", mcp.Description("Max results (default 10).")),
			),
			Handler: h.HybridSearch,
		},
		{
			Tool: mcp.NewTool("ctx_trace_relation",
				mcp.WithDescription("Trace dependency edges from a node in the knowledge graph."),
				mcp.WithString("symbol", mcp.Required(), mcp.Description("Node ID (file path or symbol).")),
				mcp.WithNumber("depth", mcp.Description("BFS depth (default 2).")),
			),
			Handler: h.TraceRelation,
		},
		{
			Tool: mcp.NewTool("ctx_quick_exec",
				mcp.WithDescription("Execute a Wasm binary in a sandbox and return summarized stdout. Only call this tool when explicitly provided with a compiled Wasm binary. Do NOT use for images, screenshots, or any non-Wasm input."),
				mcp.WithString("wasm_b64", mcp.Required(), mcp.Description("Base64-encoded compiled Wasm binary (.wasm file). Must be a valid Wasm binary, not an image or other file type.")),
				mcp.WithString("stdin", mcp.Description("Optional stdin payload.")),
			),
			Handler: h.QuickExec,
		},
		{
			Tool: mcp.NewTool("ctx_reset_index",
				mcp.WithDescription("Wipe the index and re-index from scratch. Use when the index is stale or corrupted. This is slow — only use when ctx_index_project is insufficient."),
				mcp.WithString("path", mcp.Description("Absolute path to the project root. Defaults to SIEVE_ALLOWED_ROOT.")),
			),
			Handler: h.ResetIndex,
		},
		{
			Tool: mcp.NewTool("ctx_restart_server",
				mcp.WithDescription("Restart the Sieve server process. Use when the server is unresponsive or after major configuration changes. The MCP host will restart the process automatically."),
			),
			Handler: h.RestartServer,
		},
		{
			Tool: mcp.NewTool("ctx_status",
				mcp.WithDescription("Return server status: version, uptime, node/edge counts, memory."),
			),
			Handler: h.Status,
		},
	}
}
