package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	ctxbuilder "github.com/dainbe/Sieve/internal/context"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/sandbox"
	"github.com/dainbe/Sieve/internal/store"
)

type Handler struct {
	store       *store.Store
	builder     *ctxbuilder.Builder
	pm          *indexer.ParserManager
	allowedRoot string
	version     string
	startedAt   time.Time
}

func NewHandler(s *store.Store, pm *indexer.ParserManager, allowedRoot, version string) *Handler {
	return &Handler{
		store:       s,
		builder:     ctxbuilder.NewBuilder(s),
		pm:          pm,
		allowedRoot: allowedRoot,
		version:     version,
		startedAt:   time.Now(),
	}
}

// --- ctx_build_context ---

func (h *Handler) BuildContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, ok := req.Params.Arguments["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	// Acquire read lock — blocks if IndexProject is running
	h.store.Mu.RLock()
	result, err := h.builder.Build(query)
	h.store.Mu.RUnlock()

	if err != nil {
		slog.Error("build_context: failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("build context failed: %v", err)), nil
	}

	slog.Info("build_context: done",
		"nodes", len(result.Nodes),
		"tokens", result.TokenEstimate,
		"truncated", result.Truncated,
	)

	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

// --- ctx_index_project ---

func (h *Handler) IndexProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, ok := req.Params.Arguments["path"].(string)
	if !ok || path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Acquire write lock — blocks concurrent Build calls for consistency
	h.store.Mu.Lock()
	defer h.store.Mu.Unlock()

	slog.Info("index_project: start", "path", path)
	count, err := indexer.IndexProject(ctx, h.store, h.pm, h.allowedRoot, path)
	if err != nil {
		slog.Error("index_project: failed", "path", path, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("indexing failed: %v", err)), nil
	}

	slog.Info("index_project: done", "path", path, "updated", count)
	return mcp.NewToolResultText(fmt.Sprintf("Indexed %d files (changed) from %s", count, path)), nil
}

// --- ctx_hybrid_search ---

func (h *Handler) HybridSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, ok := req.Params.Arguments["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := argInt(req.Params.Arguments["limit"], 10)

	nodes, err := h.store.FTSSearch(query, limit)
	if err != nil {
		slog.Error("hybrid_search: failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	type result struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Summary string `json:"summary"`
	}
	var results []result
	for _, n := range nodes {
		results = append(results, result{
			ID:      n.ID,
			Type:    n.Type,
			Summary: ctxbuilder.SummaryLine(n),
		})
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

// --- ctx_trace_relation ---

func (h *Handler) TraceRelation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol, ok := req.Params.Arguments["symbol"].(string)
	if !ok || symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	depth := argInt(req.Params.Arguments["depth"], 2)

	edges, err := h.store.TraceEdges(symbol, depth)
	if err != nil {
		slog.Error("trace_relation: failed", "symbol", symbol, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("trace failed: %v", err)), nil
	}

	out, _ := json.MarshalIndent(edges, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

// --- ctx_quick_exec ---

func (h *Handler) QuickExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wasmB64, ok := req.Params.Arguments["wasm_b64"].(string)
	if !ok || wasmB64 == "" {
		return mcp.NewToolResultError("wasm_b64 is required"), nil
	}
	stdin, _ := req.Params.Arguments["stdin"].(string)

	output, err := sandbox.Run(ctx, wasmB64, stdin)
	if err != nil {
		slog.Error("quick_exec: failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("exec failed: %v", err)), nil
	}
	return mcp.NewToolResultText(output), nil
}

// --- ctx_status ---

type statusResponse struct {
	Version      string  `json:"version"`
	Uptime       string  `json:"uptime"`
	NodeCount    int64   `json:"node_count"`
	EdgeCount    int64   `json:"edge_count"`
	GoVersion    string  `json:"go_version"`
	NumGoroutine int     `json:"goroutines"`
	MemAllocMB   float64 `json:"mem_alloc_mb"`
}

func (h *Handler) Status(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	nodes, edges, err := h.store.Stats()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	resp := statusResponse{
		Version:      h.version,
		Uptime:       time.Since(h.startedAt).Round(time.Second).String(),
		NodeCount:    nodes,
		EdgeCount:    edges,
		GoVersion:    runtime.Version(),
		NumGoroutine: runtime.NumGoroutine(),
		MemAllocMB:   math.Round(float64(ms.Alloc)/1024/1024*100) / 100,
	}

	out, _ := json.MarshalIndent(resp, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

func argInt(v interface{}, defaultVal int) int {
	if v == nil {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return defaultVal
}

// --- ctx_drill_down ---

func (h *Handler) DrillDown(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, ok := req.Params.Arguments["path"].(string)
	if !ok || path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Acquire read lock — consistent with Build
	h.store.Mu.RLock()
	result, err := h.builder.DrillDown(path)
	h.store.Mu.RUnlock()

	if err != nil {
		slog.Error("drill_down: failed", "path", path, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("drill down failed: %v", err)), nil
	}

	slog.Info("drill_down: done",
		"path", path,
		"nodes", len(result.Nodes),
		"tokens", result.TokenEstimate,
	)

	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}
