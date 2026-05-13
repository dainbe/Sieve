package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dainbe/Sieve/internal/store"
)

func setupTest(t *testing.T) (string, *store.Store) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sieve-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return tmpDir, s
}

// TestIndexProject_Cleanup verifies stale nodes are removed when files are deleted.
func TestIndexProject_Cleanup(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	file1 := filepath.Join(tmpDir, "file1.txt")
	if err := os.WriteFile(file1, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	// First index
	count, err := IndexProject(ctx, s, nil, "", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 updated file, got %d", count)
	}

	ids, _ := s.GetAllFileNodeIDs()
	if len(ids) != 1 || ids[0] != "file1.txt" {
		t.Errorf("expected [file1.txt], got %v", ids)
	}

	// Delete file and re-index
	if err := os.Remove(file1); err != nil {
		t.Fatal(err)
	}

	count, err = IndexProject(ctx, s, nil, "", tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 updated files, got %d", count)
	}

	ids, _ = s.GetAllFileNodeIDs()
	if len(ids) != 0 {
		t.Errorf("expected 0 nodes after deletion, got %v", ids)
	}
}

// TestIndexProject_Incremental verifies unchanged files are skipped.
func TestIndexProject_Incremental(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	file1 := filepath.Join(tmpDir, "hello.go")
	if err := os.WriteFile(file1, []byte("package main\nfunc Hello() {}"), 0644); err != nil {
		t.Fatal(err)
	}

	count, _ := IndexProject(ctx, s, nil, "", tmpDir)
	if count != 1 {
		t.Fatalf("first index: expected 1, got %d", count)
	}

	// Second index without changes — should skip.
	count, _ = IndexProject(ctx, s, nil, "", tmpDir)
	if count != 0 {
		t.Errorf("second index (no change): expected 0, got %d", count)
	}

	// Modify file — should re-index.
	if err := os.WriteFile(file1, []byte("package main\nfunc Hello() {}\nfunc World() {}"), 0644); err != nil {
		t.Fatal(err)
	}
	count, _ = IndexProject(ctx, s, nil, "", tmpDir)
	if count != 1 {
		t.Errorf("third index (modified): expected 1, got %d", count)
	}
}

// TestIndexProject_GoSymbols verifies Go AST symbol extraction.
func TestIndexProject_GoSymbols(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	src := `package main

type Server struct{}

func (s *Server) Start(addr string) error { return nil }

var Port = 8080
`
	if err := os.WriteFile(filepath.Join(tmpDir, "server.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := IndexProject(ctx, s, nil, "", tmpDir); err != nil {
		t.Fatal(err)
	}

	// Verify symbol nodes were created
	cases := []struct {
		id      string
		wantTyp string
	}{
		{"server.go:Server", "type"},
		{"server.go:Start", "function"},
		{"server.go:Port", "variable"},
	}
	for _, tc := range cases {
		n, err := s.GetNode(tc.id)
		if err != nil {
			t.Errorf("node %q not found: %v", tc.id, err)
			continue
		}
		if n.Type != tc.wantTyp {
			t.Errorf("node %q: want type %q, got %q", tc.id, tc.wantTyp, n.Type)
		}
		if n.Content == "" {
			t.Errorf("node %q: Content is empty (signature not stored)", tc.id)
		}
	}
}

// TestExtractGoSymbols_Signatures verifies Content is populated with signatures.
func TestExtractGoSymbols_Signatures(t *testing.T) {
	src := `package main

func Add(a, b int) int { return a + b }

type Point struct { X, Y float64 }

var Debug = false
`
	syms := extractGoSymbols(src)
	byName := map[string]Symbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}

	if s, ok := byName["Add"]; !ok {
		t.Error("Add not found")
	} else if s.Content == "" {
		t.Error("Add.Content is empty")
	} else if s.Type != "function" {
		t.Errorf("Add.Type: want function, got %s", s.Type)
	}

	if s, ok := byName["Point"]; !ok {
		t.Error("Point not found")
	} else if s.Type != "type" {
		t.Errorf("Point.Type: want type, got %s", s.Type)
	}

	if s, ok := byName["Debug"]; !ok {
		t.Error("Debug not found")
	} else if s.Type != "variable" {
		t.Errorf("Debug.Type: want variable, got %s", s.Type)
	}
}

// TestIndexProject_AllowedRoot verifies that files under allowedRoot are indexed
// and that symlinks pointing outside allowedRoot are rejected.
func TestIndexProject_AllowedRoot(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	// Create allowed subtree with one file.
	allowed := filepath.Join(tmpDir, "allowed")
	if err := os.Mkdir(allowed, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Index with allowedRoot set — should index the file inside allowed.
	count, err := IndexProject(ctx, s, nil, allowed, allowed)
	if err != nil {
		t.Fatalf("IndexProject failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 indexed file, got %d", count)
	}
}

// TestIndexProject_AllowedRoot_RejectsOutside verifies that a root outside
// allowedRoot is rejected before any walking occurs.
func TestIndexProject_AllowedRoot_RejectsOutside(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	allowed := filepath.Join(tmpDir, "allowed")
	outside := filepath.Join(tmpDir, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// root is outside allowedRoot — must return an error.
	_, err := IndexProject(ctx, s, nil, allowed, outside)
	if err == nil {
		t.Fatal("expected error when root is outside allowedRoot, got nil")
	}
}

// TestIndexProject_AllowedRoot_SymlinkEscape verifies that a symlink inside
// allowedRoot that points outside is silently skipped (not indexed).
func TestIndexProject_AllowedRoot_SymlinkEscape(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	allowed := filepath.Join(tmpDir, "allowed")
	outside := filepath.Join(tmpDir, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	// A legitimate file inside allowed.
	if err := os.WriteFile(filepath.Join(allowed, "ok.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A file outside allowed that a symlink will point to.
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Symlink inside allowed → outside.
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(allowed, "escape.go")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	count, err := IndexProject(ctx, s, nil, allowed, allowed)
	if err != nil {
		t.Fatalf("IndexProject failed: %v", err)
	}
	// Only ok.go should be indexed; escape.go must be skipped.
	if count != 1 {
		t.Errorf("expected 1 indexed file (symlink escape skipped), got %d", count)
	}
	ids, _ := s.GetAllFileNodeIDs()
	for _, id := range ids {
		if id == "escape.go" {
			t.Error("escape.go was indexed despite pointing outside allowedRoot")
		}
	}
}

// TestExtractImports_Go verifies Go import extraction.
func TestExtractImports_Go(t *testing.T) {
	src := `package main

import (
	"fmt"
	"os"
)
`
	imports := extractImports(src, ".go")
	want := map[string]bool{"fmt": true, "os": true}
	for _, imp := range imports {
		delete(want, imp)
	}
	if len(want) > 0 {
		t.Errorf("missing imports: %v", want)
	}
}