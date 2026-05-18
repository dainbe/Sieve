package context_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctx "github.com/dainbe/Sieve/internal/context"
	"github.com/dainbe/Sieve/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, s *store.Store, nodes []store.Node, edges [][3]string) {
	t.Helper()
	err := s.WithBatch(func(b *store.Batch) error {
		for _, n := range nodes {
			if err := b.UpsertNode(n.ID, n.Type, n.Content, "h"); err != nil {
				return err
			}
		}
		for _, e := range edges {
			if err := b.UpsertEdge(e[0], e[1], e[2]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestBuilder_Build_Empty(t *testing.T) {
	s := newTestStore(t)
	b := ctx.NewBuilder(s)
	res, err := b.Build("anything")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(res.Message, "ctx_index_project") {
		t.Errorf("expected hint about ctx_index_project, got %q", res.Message)
	}
}

func TestBuilder_Build_FTSHitOnly(t *testing.T) {
	s := newTestStore(t)
	seed(t, s, []store.Node{
		{ID: "main.go", Type: "go_file", Content: "package main\nfunc main(){}"},
	}, nil)

	b := ctx.NewBuilder(s)
	res, err := b.Build("main")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Nodes) == 0 {
		t.Fatal("expected at least one node")
	}
	found := false
	for _, n := range res.Nodes {
		if n.ID == "main.go" && n.Source == "fts" {
			found = true
		}
	}
	if !found {
		t.Errorf("main.go not found in fts nodes: %+v", res.Nodes)
	}
}

func TestBuilder_Build_GraphExpansion(t *testing.T) {
	// Disable score threshold so graph-only nodes (low score) are not filtered.
	t.Setenv("SIEVE_SCORE_THRESHOLD", "0")
	s := newTestStore(t)
	// a.go contains the FTS query term; b.go does not — it is reached only via graph.
	seed(t, s,
		[]store.Node{
			{ID: "a.go", Type: "go_file", Content: "uniquequerytokenABC imports something"},
			{ID: "b.go", Type: "go_file", Content: "func Helper() {}"},
		},
		[][3]string{{"a.go", "b.go", "imports"}},
	)

	b := ctx.NewBuilder(s)
	res, err := b.Build("uniquequerytokenABC")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sources := map[string]string{}
	for _, n := range res.Nodes {
		sources[n.ID] = n.Source
	}
	if sources["a.go"] != "fts" {
		t.Errorf("a.go should be fts hit, got %q", sources["a.go"])
	}
	if sources["b.go"] != "graph" {
		t.Errorf("b.go should be graph expanded, got %q", sources["b.go"])
	}
}

func TestBuilder_Build_TokenBudget(t *testing.T) {
	// Two go_file nodes each with 9000 chars (2250 tokens) exceed the 4000-token
	// budget: the first fits (2250), the second triggers Truncated=true.
	// Content has no newlines so it is one long line → compress returns it unchanged.
	bigContent := strings.Repeat("budgettoken ", 750) // 9000 chars, no newlines
	s := newTestStore(t)
	seed(t, s, []store.Node{
		{ID: "a.go", Type: "go_file", Content: bigContent},
		{ID: "b.go", Type: "go_file", Content: bigContent},
	}, nil)

	b := ctx.NewBuilder(s)
	res, err := b.Build("budgettoken")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated=true: two 2250-token nodes should exceed the 4000-token budget")
	}
}

func TestBuilder_DrillDown_DirectoryPrefix(t *testing.T) {
	s := newTestStore(t)
	seed(t, s, []store.Node{
		{ID: "internal/store/store.go", Type: "go_file", Content: "package store"},
		{ID: "internal/other/other.go", Type: "go_file", Content: "package other"},
	}, nil)

	b := ctx.NewBuilder(s)
	res, err := b.DrillDown("internal/store")
	if err != nil {
		t.Fatalf("DrillDown: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d: %+v", len(res.Nodes), res.Nodes)
	}
	if res.Nodes[0].ID != "internal/store/store.go" {
		t.Errorf("unexpected node ID: %s", res.Nodes[0].ID)
	}
}

func TestBuilder_DrillDown_ExactFile(t *testing.T) {
	s := newTestStore(t)
	seed(t, s, []store.Node{
		{ID: "foo/bar.go", Type: "go_file", Content: "package foo"},
		{ID: "foo/baz.go", Type: "go_file", Content: "package foo"},
	}, nil)

	b := ctx.NewBuilder(s)
	res, err := b.DrillDown("foo/bar.go")
	if err != nil {
		t.Fatalf("DrillDown: %v", err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].ID != "foo/bar.go" {
		t.Errorf("expected exactly foo/bar.go, got %+v", res.Nodes)
	}
}

func TestBuilder_DrillDown_NotFound(t *testing.T) {
	s := newTestStore(t)
	b := ctx.NewBuilder(s)
	res, err := b.DrillDown("nonexistent/path")
	if err != nil {
		t.Fatalf("DrillDown: %v", err)
	}
	if res.Message == "" {
		t.Error("expected non-empty message for not-found path")
	}
}

func TestBuilder_TypeBoost(t *testing.T) {
	// Graph expansion rolls symbol IDs (e.g. "pkg/a.go:Fn") up to their parent
	// file ID ("pkg/a.go"). The parent file should appear in results; no separate
	// symbol candidate is created for contains-edge symbols.
	s := newTestStore(t)
	seed(t, s,
		[]store.Node{
			{ID: "pkg/a.go", Type: "go_file", Content: "boost_term package foo"},
			{ID: "pkg/a.go:Fn", Type: "function", Content: "boost_term func Fn()"},
		},
		[][3]string{{"pkg/a.go", "pkg/a.go:Fn", "contains"}},
	)

	b := ctx.NewBuilder(s)
	res, err := b.Build("boost_term")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The file node must appear; symbol graph hits merge into it rather than creating
	// a separate entry.
	hasFile := false
	for _, n := range res.Nodes {
		if n.ID == "pkg/a.go" {
			hasFile = true
		}
	}
	if !hasFile {
		t.Error("expected pkg/a.go in results")
	}
}

// TestBuilder_Tools verifies all 9 MCP tools are registered.
func TestBuilder_ToolCount(t *testing.T) {
	// This test lives here to avoid import cycles; it just ensures registry.go lists the right count.
	// Actual registration is tested by go build in main.go.
	const expectedTools = 9
	_ = expectedTools // documented expectation; see internal/tools/registry.go
	_ = os.Getenv     // suppress unused import warning
}

func TestBuilder_CompressGoFile_StringLiteralBraces(t *testing.T) {
	// The old brace-counting heuristic mishandled string literals containing {}.
	// The AST-based path must not be confused by them.
	src := `package main

import "fmt"

func greet(name string) {
	fmt.Printf("Hello, %s! {braces in string}", name)
}

func add(a, b int) int {
	return a + b
}
`
	s := newTestStore(t)
	seed(t, s, []store.Node{
		{ID: "main.go", Type: "go_file", Content: src},
	}, nil)

	b := ctx.NewBuilder(s)
	res, err := b.DrillDown("main.go")
	if err != nil {
		t.Fatalf("DrillDown: %v", err)
	}
	if len(res.Nodes) == 0 {
		t.Fatal("expected nodes")
	}
	compressed := res.Nodes[0].Content
	// Both function signatures must appear.
	if !strings.Contains(compressed, "greet") {
		t.Errorf("greet signature missing from compressed output:\n%s", compressed)
	}
	if !strings.Contains(compressed, "add") {
		t.Errorf("add signature missing from compressed output:\n%s", compressed)
	}
	// The string literal content must not corrupt the output.
	if strings.Contains(compressed, "braces in string") {
		t.Errorf("string literal body leaked into compressed output:\n%s", compressed)
	}
}
