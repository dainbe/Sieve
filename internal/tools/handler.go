package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	ctxbuilder "github.com/dainbe/Sieve/internal/context"
	"github.com/dainbe/Sieve/internal/embed"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/sandbox"
	"github.com/dainbe/Sieve/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	maxQueryLen  = 4096            // max length of query / symbol parameters
	maxStdinSize = 1 * 1024 * 1024 // max stdin payload for QuickExec (1 MiB)
)

type Handler struct {
	store       *store.Store
	builder     *ctxbuilder.Builder
	pm          *indexer.ParserManager
	idx         *indexer.Indexer // may be nil when auto-index is disabled
	allowedRoot string
	parsersDir  string // empty when SIEVE_PARSERS_DIR is not set
	version     string
	startedAt   time.Time
	restartCh   chan struct{}
}

// NewHandler constructs a Handler. An optional restartCh may be supplied so
// that RestartServer signals the main loop instead of calling os.Exit.
func NewHandler(s *store.Store, pm *indexer.ParserManager, allowedRoot, parsersDir, version string, restartCh ...chan struct{}) *Handler {
	var ch chan struct{}
	if len(restartCh) > 0 {
		ch = restartCh[0]
	}
	return &Handler{
		store:       s,
		builder:     ctxbuilder.NewBuilder(s),
		pm:          pm,
		allowedRoot: allowedRoot,
		parsersDir:  parsersDir,
		version:     version,
		startedAt:   time.Now(),
		restartCh:   ch,
	}
}

// SetIndexer attaches an Indexer so ctx_status can report live progress.
func (h *Handler) SetIndexer(ix *indexer.Indexer) {
	h.idx = ix
}

// SetEmbedder enables dense semantic retrieval on the builder.
// Call after BulkEmbed completes (or on startup if vectors already exist).
func (h *Handler) SetEmbedder(emb *embed.Embedder, idx *embed.VectorIndex) {
	h.builder.SetEmbedder(emb, idx)
}

// withinAllowedRoot は path が allowedRoot 配下かを検証する。
// allowedRoot が空のとき（main.go の必須チェックが通った想定）は常に true。
func (h *Handler) withinAllowedRoot(path string) bool {
	if h.allowedRoot == "" {
		return true
	}
	clean := filepath.Clean(path)
	root := filepath.Clean(h.allowedRoot)
	return clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))
}

// --- ctx_build_context ---

func (h *Handler) BuildContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	if len(query) > maxQueryLen {
		return mcp.NewToolResultError(fmt.Sprintf("query too long (max %d bytes)", maxQueryLen)), nil
	}

	result, err := h.builder.Build(query)
	if err != nil {
		slog.Error("build_context: failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("build context failed: %v", err)), nil
	}

	slog.Info("build_context: done",
		"nodes", len(result.Nodes),
		"tokens", result.TokenEstimate,
		"truncated", result.Truncated,
	)

	out, err := marshalJSON(result)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

// --- ctx_index_project ---

func (h *Handler) IndexProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		path = h.allowedRoot
	}
	if path == "" {
		return mcp.NewToolResultError("path is required when SIEVE_ALLOWED_ROOT is not set"), nil
	}
	// If the client passes a relative path (e.g. ".") and allowedRoot is set,
	// use allowedRoot directly — filepath.Abs resolves against the process
	// working directory which may differ from the project root.
	if !filepath.IsAbs(path) {
		if h.allowedRoot != "" {
			path = h.allowedRoot
		} else {
			abs, err := filepath.Abs(path)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("cannot resolve path %q: %v", path, err)), nil
			}
			path = abs
		}
	}

	if !h.withinAllowedRoot(path) {
		return mcp.NewToolResultError(fmt.Sprintf("path %q is outside the allowed root %q", path, h.allowedRoot)), nil
	}

	slog.Info("index_project: start", "path", path)
	count, err := indexer.IndexProject(ctx, h.store, h.pm, h.allowedRoot, path)
	if err != nil {
		slog.Error("index_project: failed", "path", path, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("indexing failed: %v", err)), nil
	}

	slog.Info("index_project: done", "path", path, "updated", count)
	return mcp.NewToolResultText(fmt.Sprintf("Indexed %d files (changed) from %s", count, path)), nil
}

// --- ctx_reset_index ---

func (h *Handler) ResetIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	confirm := req.GetString("confirm", "")
	if confirm != "yes-delete-all" {
		return mcp.NewToolResultError(`ctx_reset_index requires confirm="yes-delete-all" to prevent accidental data loss`), nil
	}
	if h.idx != nil && h.idx.IndexingActive() {
		return mcp.NewToolResultError("cannot reset index while indexing is in progress; wait for indexing to complete or restart the server"), nil
	}

	path := req.GetString("path", "")
	if path == "" {
		path = h.allowedRoot
	}
	if path == "" {
		return mcp.NewToolResultError("path is required when SIEVE_ALLOWED_ROOT is not set"), nil
	}
	if !filepath.IsAbs(path) {
		if h.allowedRoot != "" {
			path = h.allowedRoot
		} else {
			abs, err := filepath.Abs(path)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("cannot resolve path %q: %v", path, err)), nil
			}
			path = abs
		}
	}

	if !h.withinAllowedRoot(path) {
		return mcp.NewToolResultError(fmt.Sprintf("path %q is outside the allowed root %q", path, h.allowedRoot)), nil
	}

	slog.Info("reset_index: clearing store")
	if err := h.store.Reset(); err != nil {
		slog.Error("reset_index: reset failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("reset failed: %v", err)), nil
	}

	slog.Info("reset_index: re-indexing", "path", path)
	count, err := indexer.IndexProject(ctx, h.store, h.pm, h.allowedRoot, path)
	if err != nil {
		slog.Error("reset_index: indexing failed", "path", path, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("indexing failed: %v", err)), nil
	}

	slog.Info("reset_index: done", "path", path, "indexed", count)
	return h.buildStatusResult()
}

// --- ctx_restart_server ---

func (h *Handler) RestartServer(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slog.Info("restart_server: signaling main loop to restart")
	if h.restartCh != nil {
		select {
		case h.restartCh <- struct{}{}:
		default: // channel full, restart already pending
		}
	}
	return mcp.NewToolResultText("Sieve is restarting. Wait a moment then retry your request."), nil
}

// --- ctx_hybrid_search ---

func (h *Handler) HybridSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	if len(query) > maxQueryLen {
		return mcp.NewToolResultError(fmt.Sprintf("query too long (max %d bytes)", maxQueryLen)), nil
	}
	limit := req.GetInt("limit", 10)
	if limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
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

	out, err := marshalJSON(results)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

// --- ctx_trace_relation ---

func (h *Handler) TraceRelation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbol := req.GetString("symbol", "")
	if symbol == "" {
		return mcp.NewToolResultError("symbol is required"), nil
	}
	if len(symbol) > maxQueryLen {
		return mcp.NewToolResultError(fmt.Sprintf("symbol too long (max %d bytes)", maxQueryLen)), nil
	}
	depth := req.GetInt("depth", 2)
	if depth < 1 {
		depth = 2
	}
	if depth > 5 {
		depth = 5
	}
	edges, err := h.store.TraceEdges(symbol, depth)
	if err != nil {
		slog.Error("trace_relation: failed", "symbol", symbol, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("trace failed: %v", err)), nil
	}

	out, err := marshalJSON(edges)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

// --- ctx_quick_exec ---

func (h *Handler) QuickExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	wasmB64 := req.GetString("wasm_b64", "")
	if wasmB64 == "" {
		return mcp.NewToolResultError("wasm_b64 is required"), nil
	}
	if len(wasmB64) > 10*1024*1024 {
		return mcp.NewToolResultError("wasm_b64 too large (max 10 MB base64)"), nil
	}
	stdin := req.GetString("stdin", "")
	if len(stdin) > maxStdinSize {
		return mcp.NewToolResultError(fmt.Sprintf("stdin too large (max %d bytes)", maxStdinSize)), nil
	}

	output, err := sandbox.Run(ctx, wasmB64, stdin)
	if err != nil {
		slog.Error("quick_exec: failed", "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("exec failed: %v", err)), nil
	}
	return mcp.NewToolResultText(output), nil
}

// --- ctx_status ---

type statusResponse struct {
	Version          string  `json:"version"`
	Uptime           string  `json:"uptime"`
	IndexedFiles     int64   `json:"indexed_files"` // alias for node_count for clarity
	NodeCount        int64   `json:"node_count"`
	EdgeCount        int64   `json:"edge_count"`
	GoVersion        string  `json:"go_version"`
	NumGoroutine     int     `json:"goroutines"`
	MemAllocMB       float64 `json:"mem_alloc_mb"`
	DBPath           string  `json:"db_path"`
	AllowedRoot      string  `json:"allowed_root"`
	Indexing         bool    `json:"indexing"`
	IndexingProgress string  `json:"indexing_progress,omitempty"`
	Embedding        bool    `json:"embedding"`
	EmbeddedFiles    int32   `json:"embedded_files"`
	LastIndexError   string  `json:"last_index_error,omitempty"`
}

func (h *Handler) buildStatusResult() (*mcp.CallToolResult, error) {
	nodes, edges, err := h.store.Stats()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	resp := statusResponse{
		Version:      h.version,
		Uptime:       time.Since(h.startedAt).Round(time.Second).String(),
		IndexedFiles: nodes,
		NodeCount:    nodes,
		EdgeCount:    edges,
		GoVersion:    runtime.Version(),
		NumGoroutine: runtime.NumGoroutine(),
		MemAllocMB:   math.Round(float64(ms.Alloc)/1024/1024*100) / 100,
		DBPath:       h.store.Path(),
		AllowedRoot:  h.allowedRoot,
	}

	if h.idx != nil {
		resp.Indexing = h.idx.IndexingActive()
		done, total := h.idx.IndexingProgress()
		if total > 0 {
			resp.IndexingProgress = fmt.Sprintf("%d/%d", done, total)
		}
		resp.Embedding = h.idx.EmbeddingActive()
		resp.EmbeddedFiles = h.idx.EmbeddedFiles()
		resp.LastIndexError = h.idx.LastIndexError()
	}

	out, err := marshalJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

func (h *Handler) Status(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.buildStatusResult()
}

// marshalJSON serialises v to indented JSON.
// Marshalling our own structs should never fail; if it does, a safe fallback
// error message is returned so the caller can still respond.
func marshalJSON(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal response: %w", err)
	}
	return string(out), nil
}

// --- ctx_init ---

type initResponse struct {
	Ready          bool     `json:"ready"`
	IndexedFiles   int64    `json:"indexed_files"`
	NewlyIndexed   int      `json:"newly_indexed"`
	Optimized      bool     `json:"optimized"`
	Indexing       bool     `json:"indexing,omitempty"`
	IndexProgress  string   `json:"indexing_progress,omitempty"`
	ParsersFetched []string `json:"parsers_fetched,omitempty"`
	Message        string   `json:"message"`
	Warnings       []string `json:"warnings,omitempty"`
}

func (h *Handler) Init(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// If background indexing is in progress, report and ask to retry.
	if h.idx != nil && h.idx.IndexingActive() {
		done, total := h.idx.IndexingProgress()
		prog := ""
		if total > 0 {
			prog = fmt.Sprintf("%d/%d", done, total)
		}
		resp := initResponse{
			Ready:         false,
			Indexing:      true,
			IndexProgress: prog,
			Message:       "Indexing is in progress. Call ctx_init again when complete.",
		}
		out, err := marshalJSON(resp)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(out), nil
	}

	nodes, _, err := h.store.Stats()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	var newlyIndexed int
	var warnings []string
	var parsersFetched []string

	// Fetch missing Wasm parsers before indexing so they are used immediately.
	if h.parsersDir != "" {
		fetched, fetchErrs := indexer.FetchMissingParsers(ctx, h.parsersDir)
		parsersFetched = fetched
		for _, e := range fetchErrs {
			warnings = append(warnings, "parser-fetch: "+e)
		}
	}

	// Build index if empty.
	if nodes == 0 && h.allowedRoot != "" {
		slog.Info("init: index empty, indexing now", "root", h.allowedRoot)
		count, idxErr := indexer.IndexProject(ctx, h.store, h.pm, h.allowedRoot, h.allowedRoot)
		if idxErr != nil {
			slog.Warn("init: indexing failed", "err", idxErr)
			warnings = append(warnings, fmt.Sprintf("indexing: %v", idxErr))
		} else {
			newlyIndexed = count
			slog.Info("init: indexing complete", "indexed", count)
		}
		// Refresh node count after indexing.
		nodes, _, _ = h.store.Stats()
	}

	// Run SQLite optimization.
	optimized := false
	if optErr := h.store.Optimize(); optErr != nil {
		slog.Warn("init: optimize failed", "err", optErr)
		warnings = append(warnings, fmt.Sprintf("optimize: %v", optErr))
	} else {
		optimized = true
	}

	msg := "Sieve is ready. Use ctx_build_context to query the codebase."
	if newlyIndexed > 0 {
		msg = fmt.Sprintf("Indexed %d files and optimized the database. Use ctx_build_context to query.", newlyIndexed)
	} else if nodes == 0 {
		msg = "Index is empty and SIEVE_ALLOWED_ROOT is not set. Call ctx_index_project with an explicit path."
	}

	resp := initResponse{
		Ready:          nodes > 0 || newlyIndexed > 0,
		IndexedFiles:   nodes,
		NewlyIndexed:   newlyIndexed,
		Optimized:      optimized,
		ParsersFetched: parsersFetched,
		Message:        msg,
		Warnings:       warnings,
	}

	slog.Info("init: done",
		"indexed_files", nodes,
		"newly_indexed", newlyIndexed,
		"optimized", optimized,
	)

	out, err := marshalJSON(resp)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

// --- ctx_drill_down ---

func (h *Handler) DrillDown(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}
	if len(path) > 4096 {
		return mcp.NewToolResultError("path too long (max 4096 bytes)"), nil
	}
	result, err := h.builder.DrillDown(path)
	if err != nil {
		slog.Error("drill_down: failed", "path", path, "err", err)
		return mcp.NewToolResultError(fmt.Sprintf("drill down failed: %v", err)), nil
	}

	slog.Info("drill_down: done",
		"path", path,
		"nodes", len(result.Nodes),
		"tokens", result.TokenEstimate,
	)

	out, err := marshalJSON(result)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}
