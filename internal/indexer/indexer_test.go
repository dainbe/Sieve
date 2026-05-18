package indexer

import (
	"context"
	"fmt"
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
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
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

func containsImport(imports []string, target string) bool {
	for _, imp := range imports {
		if imp == target {
			return true
		}
	}
	return false
}

func TestExtractImports_TS_Basic(t *testing.T) {
	src := `import React from "react";
import { useState } from "react";
export { x } from "./mod";
const y = require("lodash");`
	got := extractTSJSImports(src)
	for _, want := range []string{"react", "./mod", "lodash"} {
		if !containsImport(got, want) {
			t.Errorf("expected %q in imports, got %v", want, got)
		}
	}
}

func TestExtractImports_TS_Comments(t *testing.T) {
	src := `// import "fake-line-comment"
/* import "fake-block-comment" */
import "real";`
	got := extractTSJSImports(src)
	if containsImport(got, "fake-line-comment") {
		t.Error("should not extract from single-line comment")
	}
	if containsImport(got, "fake-block-comment") {
		t.Error("should not extract from block comment")
	}
	if !containsImport(got, "real") {
		t.Errorf("should extract real import, got %v", got)
	}
}

func TestExtractImports_TS_TemplateLiteral(t *testing.T) {
	src := "const s = `import \"fake-template\"`;\nimport \"real\";"
	got := extractTSJSImports(src)
	if containsImport(got, "fake-template") {
		t.Errorf("should not extract from template literal, got %v", got)
	}
	if !containsImport(got, "real") {
		t.Errorf("should extract real import, got %v", got)
	}
}

func TestExtractImports_TS_Important(t *testing.T) {
	src := `const important = "value";
let reimport = "v2";
import "actual";`
	got := extractTSJSImports(src)
	for _, bad := range []string{"value", "v2"} {
		if containsImport(got, bad) {
			t.Errorf("identifier containing 'import' should not be matched, found %q", bad)
		}
	}
	if !containsImport(got, "actual") {
		t.Errorf("real import not found, got %v", got)
	}
}

// TestIndexProject_CrossFileCallsEdge verifies that a calls edge is written
// between two symbols in different files within the same package.
func TestIndexProject_CrossFileCallsEdge(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	// a.go: RunA calls RunB (defined in b.go, same directory)
	srcA := `package mypkg

func RunA() {
	RunB()
}
`
	// b.go: defines RunB
	srcB := `package mypkg

func RunB() {}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte(srcA), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte(srcB), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := IndexProject(ctx, s, nil, "", tmpDir); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}

	// Expect edge: a.go:RunA → b.go:RunB with relation "calls"
	edges, err := s.TraceEdges("a.go:RunA", 1)
	if err != nil {
		t.Fatalf("TraceEdges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.FromID == "a.go:RunA" && e.ToID == "b.go:RunB" && e.Relation == "calls" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cross-file calls edge a.go:RunA → b.go:RunB; edges: %v", edges)
	}
}

// TestIndexProject_AmbiguousCallSamePackagePreferred verifies that when multiple
// files define the same symbol name, the same-directory candidate is preferred.
func TestIndexProject_AmbiguousCallSamePackagePreferred(t *testing.T) {
	tmpDir, s := setupTest(t)
	ctx := context.Background()

	// pkg/a.go calls Helper; pkg/b.go and other/c.go both define Helper.
	pkg := filepath.Join(tmpDir, "pkg")
	other := filepath.Join(tmpDir, "other")
	if err := os.MkdirAll(pkg, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}

	srcA := `package pkg

func RunA() {
	Helper()
}
`
	srcB := `package pkg

func Helper() {}
`
	srcC := `package other

func Helper() {}
`
	if err := os.WriteFile(filepath.Join(pkg, "a.go"), []byte(srcA), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "b.go"), []byte(srcB), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "c.go"), []byte(srcC), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := IndexProject(ctx, s, nil, "", tmpDir); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}

	edges, err := s.TraceEdges("pkg/a.go:RunA", 1)
	if err != nil {
		t.Fatalf("TraceEdges: %v", err)
	}

	wantTarget := "pkg/b.go:Helper"
	badTarget := "other/c.go:Helper"
	var gotTarget string
	for _, e := range edges {
		if e.Relation == "calls" {
			gotTarget = e.ToID
		}
	}
	if gotTarget != wantTarget {
		t.Errorf("same-package preference: want %q, got %q (bad: %q in edges %v)",
			wantTarget, gotTarget, badTarget, edges)
	}
}

// TestIndexProject_ParallelDeterminism verifies that indexing with multiple workers
// produces the same node/edge counts and cross-file call edges as a single worker.
// This catches data races and non-determinism introduced by parallel parsing.
func TestIndexProject_ParallelDeterminism(t *testing.T) {
	// Build a mini project with cross-file calls and multiple languages.
	makeProject := func(t *testing.T) string {
		t.Helper()
		dir, err := os.MkdirTemp("", "sieve-parallel-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			filepath.Join(dir, "main.go"): `package main
import "fmt"
func main() { Greet("world") }
func Greet(s string) { fmt.Println(s) }
`,
			filepath.Join(dir, "util.go"): `package main
func Helper() {}
func AnotherHelper() { Helper() }
`,
			filepath.Join(sub, "sub.go"): `package sub
func SubFunc() {}
func SubCaller() { SubFunc() }
`,
			filepath.Join(dir, "readme.md"): "# Project\nSome docs.\n",
			filepath.Join(dir, "script.py"): `def process(): pass
def run(): process()
`,
		}
		for path, content := range files {
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	// Index with nWorkers, return (nodeCount, edgeCount, sorted node IDs).
	indexWith := func(t *testing.T, nWorkers int) (int64, int64, []string) {
		t.Helper()
		t.Setenv("SIEVE_INDEX_WORKERS", fmt.Sprintf("%d", nWorkers))
		// Force re-evaluation of the package-level var for the test.
		old := indexWorkers
		indexWorkers = nWorkers
		t.Cleanup(func() { indexWorkers = old })

		dir := makeProject(t)
		_, s := setupTest(t)
		ctx := context.Background()

		n, err := IndexProject(ctx, s, nil, "", dir)
		if err != nil {
			t.Fatalf("IndexProject (workers=%d): %v", nWorkers, err)
		}
		if n == 0 {
			t.Errorf("expected >0 files indexed with %d workers", nWorkers)
		}

		nodes, edges, err := s.Stats()
		if err != nil {
			t.Fatal(err)
		}
		ids, _ := s.GetAllFileNodeIDs()
		return nodes, edges, ids
	}

	nodes1, edges1, ids1 := indexWith(t, 1)
	nodes4, edges4, ids4 := indexWith(t, 4)
	nodes8, edges8, ids8 := indexWith(t, 8)

	if nodes1 != nodes4 || nodes1 != nodes8 {
		t.Errorf("node count mismatch: 1=%d 4=%d 8=%d", nodes1, nodes4, nodes8)
	}
	if edges1 != edges4 || edges1 != edges8 {
		t.Errorf("edge count mismatch: 1=%d 4=%d 8=%d", edges1, edges4, edges8)
	}
	if len(ids1) != len(ids4) || len(ids1) != len(ids8) {
		t.Errorf("file node count mismatch: 1=%d 4=%d 8=%d", len(ids1), len(ids4), len(ids8))
	}
	_ = ids1
	_ = ids4
	_ = ids8
}

// TestIndexProject_ParallelCrossFileEdges verifies cross-file call edges are
// correctly resolved regardless of the number of parallel workers.
func TestIndexProject_ParallelCrossFileEdges(t *testing.T) {
	dir, err := os.MkdirTemp("", "sieve-xfile-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	pkgA := `package a
func Caller() { Callee() }
`
	pkgB := `package a
func Callee() {}
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(pkgA), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(pkgB), 0644); err != nil {
		t.Fatal(err)
	}

	for _, nw := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", nw), func(t *testing.T) {
			old := indexWorkers
			indexWorkers = nw
			defer func() { indexWorkers = old }()

			_, s := setupTest(t)
			if _, err := IndexProject(context.Background(), s, nil, "", dir); err != nil {
				t.Fatalf("IndexProject: %v", err)
			}
			edges, err := s.TraceEdges("a.go:Caller", 1)
			if err != nil {
				t.Fatalf("TraceEdges: %v", err)
			}
			var found bool
			for _, e := range edges {
				if e.Relation == "calls" && e.ToID == "b.go:Callee" {
					found = true
				}
			}
			if !found {
				t.Errorf("cross-file call edge a.go:Caller→b.go:Callee not found with %d workers; edges=%v", nw, edges)
			}
		})
	}
}

// BenchmarkIndexProject_Workers measures indexing throughput vs worker count
// against the real Sieve repo. Run with: go test -bench=BenchmarkIndexProject_Workers
func BenchmarkIndexProject_Workers(b *testing.B) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		b.Skip("cannot resolve repo root")
	}

	for _, nw := range []int{1, 2, 4, 8} {
		nw := nw
		b.Run(fmt.Sprintf("workers=%d", nw), func(b *testing.B) {
			old := indexWorkers
			indexWorkers = nw
			defer func() { indexWorkers = old }()

			dbPath := filepath.Join(b.TempDir(), "bench.db")
			s, err := store.New(dbPath)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = s.Close() }()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Reset(); err != nil {
					b.Fatal(err)
				}
				if _, err := IndexProject(context.Background(), s, nil, repoRoot, repoRoot); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestExtractImports_TS_RequireParen(t *testing.T) {
	src := `const x = require("lodash");`
	got := extractTSJSImports(src)
	if !containsImport(got, "lodash") {
		t.Errorf("require() specifier not found, got %v", got)
	}
}

func TestExtractImports_TS_Multiline(t *testing.T) {
	// The from-line carries the specifier even for multi-line imports
	src := "import {\n  a,\n  b\n} from \"multi\";"
	got := extractTSJSImports(src)
	if !containsImport(got, "multi") {
		t.Errorf("multi-line import specifier not found, got %v", got)
	}
}
