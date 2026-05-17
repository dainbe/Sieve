package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
	"github.com/dainbe/Sieve/internal/tools"
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

	s.AddTools(h.Tools()...)

	slog.Info("Sieve MCP server starting", "version", version, "db", dbPath, "allowed_root", allowedRoot)
	if err := server.ServeStdio(s); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
