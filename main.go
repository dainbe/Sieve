package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/dainbe/Sieve/internal/embed"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
	"github.com/dainbe/Sieve/internal/tools"
	"github.com/mark3labs/mcp-go/server"
)

const version = "0.1.0"

// embedAdapter bridges embed.Embedder (batch API) to indexer.Embedder (single-text API).
type embedAdapter struct{ e *embed.Embedder }

func (a embedAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.e.EmbedOne(ctx, text)
}

type envVar struct {
	Name     string
	Default  string
	Desc     string
	Required bool
}

var envVars = []envVar{
	{"SIEVE_ALLOWED_ROOT", "", "Absolute path to the project root to index and serve.", true},
	{"SIEVE_AUTO_INDEX", "1", "Set to 0 to disable automatic background indexing on startup.", false},
	{"SIEVE_INDEX_WORKERS", "", "Parallel file-parse workers (default: runtime.NumCPU()).", false},
	{"SIEVE_DENSE_RETRIEVAL", "0", "Set to 1 to enable dense (embedding-based) retrieval.", false},
	{"SIEVE_MAX_FILE_BYTES", "2097152", "Skip files larger than this many bytes (default 2 MiB).", false},
	{"SIEVE_GRAPH_DEPTH", "3", "BFS depth for graph expansion (0 = disable graph).", false},
	{"SIEVE_GRAPH_SEED_TOP_K", "30", "Max seed nodes for graph expansion.", false},
	{"SIEVE_FTS_FILE_LIMIT", "200", "Max files returned by FTS search.", false},
	{"SIEVE_SCORE_THRESHOLD", "0.25", "Minimum score to include a node in context.", false},
	{"SIEVE_QUERY_EXPANSION", "0", "PPMI neighbor count for query expansion (0 = disable).", false},
	{"SIEVE_PPMI_MIN_COUNT", "2", "Minimum co-occurrence count for PPMI.", false},
	{"SIEVE_PPMI_DISABLE", "0", "Set to 1 to disable PPMI entirely.", false},
	{"SIEVE_PPMI_REBUILD_THRESHOLD", "100", "Skip PPMI rebuild if fewer than N files changed.", false},
	{"SIEVE_PARSERS_DIR", "", "Directory containing Wasm parser binaries.", false},
	{"SIEVE_DEBUG", "0", "Set to 1 to enable debug-level logging.", false},
	{"SIEVE_EVAL_DUMP", "0", "Set to 1 to dump eval context output.", false},
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h":
			printHelp()
			os.Exit(0)
		case "--version":
			printVersion()
			os.Exit(0)
		case "--config":
			printConfig()
			os.Exit(0)
		}
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println("Sieve MCP server — AI coding-agent context optimizer")
	fmt.Println()
	fmt.Printf("Usage: %s [--help | --version | --config]\n", os.Args[0])
	fmt.Println("       (no flags) — start the MCP server in stdio mode")
	fmt.Println()
	fmt.Println("Environment variables:")
	fmt.Println()
	for _, v := range envVars {
		def := v.Default
		label := "default"
		if v.Required {
			def = "(required)"
			label = "        "
		} else if def == "" {
			def = "(none)"
		}
		fmt.Printf("  %-30s  %s: %-12s  %s\n", v.Name, label, def, v.Desc)
	}
	fmt.Println()
}

func printVersion() {
	goVersion := "unknown"
	vcsRevision := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		goVersion = info.GoVersion
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				vcsRevision = s.Value
			}
		}
	}
	fmt.Printf("sieve %s\n", version)
	fmt.Printf("go:   %s\n", goVersion)
	fmt.Printf("vcs:  %s\n", vcsRevision)
}

func printConfig() {
	cfg := map[string]string{}
	for _, v := range envVars {
		val := os.Getenv(v.Name)
		switch {
		case val != "":
			cfg[v.Name] = val
		case v.Required:
			cfg[v.Name] = "(required, not set)"
		case v.Default != "":
			cfg[v.Name] = v.Default
		default:
			cfg[v.Name] = "(none)"
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(cfg)
}

func run() error {
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
		return fmt.Errorf("SIEVE_ALLOWED_ROOT is required; set it to the project root directory")
	}

	// --- Store: DB is always placed inside the project root ---
	// {SIEVE_ALLOWED_ROOT}/.sieve/sieve.db
	dbPath := filepath.Join(allowedRoot, ".sieve", "sieve.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("create db directory: %w", err)
	}
	db, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("store init: %w", err)
	}
	defer func() { _ = db.Close() }()

	// --- Parser manager (optional) ---
	parsersDir := os.Getenv("SIEVE_PARSERS_DIR")
	var pm *indexer.ParserManager
	if parsersDir != "" {
		pm, err = indexer.NewParserManager(parsersDir)
		if err != nil {
			slog.Warn("parser manager init failed", "err", err)
		} else {
			defer func() { _ = pm.Close() }()
		}
	}

	// --- Signal context (SIGINT / SIGTERM) ---
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- Dense retrieval embedder (optional, fail-open) ---
	var embImpl *embed.Embedder
	if os.Getenv("SIEVE_DENSE_RETRIEVAL") == "1" {
		var embErr error
		embImpl, embErr = embed.New(ctx)
		if embErr != nil {
			slog.Warn("dense retrieval: embedder init failed", "err", embErr)
			embImpl = nil
		} else {
			defer embImpl.Close()
		}
	}
	var emb indexer.Embedder
	if embImpl != nil {
		emb = embedAdapter{embImpl}
	}

	// --- Indexer with progress tracking ---
	ix := indexer.NewIndexer(db, pm, allowedRoot, emb)

	// --- Restart channel ---
	restartCh := make(chan struct{}, 1)

	// --- MCP server ---
	s := server.NewMCPServer("sieve", version)
	h := tools.NewHandler(db, pm, allowedRoot, parsersDir, version, restartCh)
	h.SetIndexer(ix)

	// Enable dense retrieval immediately if vectors already exist from a prior run.
	if embImpl != nil {
		if vecs, vErr := db.LoadAllVectors(); vErr == nil && len(vecs) > 0 {
			h.SetEmbedder(embImpl, embed.NewVectorIndex(vecs))
			slog.Info("dense retrieval: loaded existing vectors", "count", len(vecs))
		}
	}

	// --- Auto-index on startup (A.1) ---
	if os.Getenv("SIEVE_AUTO_INDEX") != "0" {
		go func() {
			ix.IndexAll(ctx)
			if embImpl != nil {
				if err := ix.BulkEmbed(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("bulk-embed: error", "err", err)
					return
				}
				// Rebuild vector index after embedding completes.
				if vecs, vErr := db.LoadAllVectors(); vErr == nil && len(vecs) > 0 {
					h.SetEmbedder(embImpl, embed.NewVectorIndex(vecs))
					slog.Info("dense retrieval: vector index ready", "count", len(vecs))
				}
			}
		}()
	}

	tools.RegisterTools(s, h)

	slog.Info("Sieve MCP server starting", "version", version, "db", dbPath, "allowed_root", allowedRoot)

	// Run ServeStdio in a goroutine so we can also watch for signals / restart.
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ServeStdio(s)
	}()

	select {
	case <-ctx.Done():
		slog.Info("Sieve: shutting down (signal)")
	case <-restartCh:
		slog.Info("Sieve: restarting on request")
	case err := <-serveDone:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
		return nil
	}
	// defer db.Close() and pm.Close() execute here before process exits.
	return nil
}
