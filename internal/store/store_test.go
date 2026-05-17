package store_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

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

func TestStore_UpsertAndGetNode(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertNode("foo.go", "go_file", "package foo", "abc123"); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	if !s.Exists("foo.go") {
		t.Fatal("expected node to exist")
	}
	n, err := s.GetNode("foo.go")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if n.ID != "foo.go" || n.Type != "go_file" || n.Content != "package foo" {
		t.Errorf("unexpected node: %+v", n)
	}
}

func TestStore_WithBatch_Commit(t *testing.T) {
	s := newTestStore(t)
	err := s.WithBatch(func(b *store.Batch) error {
		if err := b.UpsertNode("a.go", "go_file", "package a", "h1"); err != nil {
			return err
		}
		if err := b.UpsertNode("b.go", "go_file", "package b", "h2"); err != nil {
			return err
		}
		return b.UpsertEdge("a.go", "b.go", "imports")
	})
	if err != nil {
		t.Fatalf("WithBatch: %v", err)
	}
	if !s.Exists("a.go") || !s.Exists("b.go") {
		t.Error("nodes should exist after commit")
	}
	nodes, _ := s.TraceNodeIDs("a.go", 1)
	found := false
	for _, id := range nodes {
		if id == "b.go" {
			found = true
		}
	}
	if !found {
		t.Error("edge should exist after commit")
	}
}

func TestStore_WithBatch_Rollback(t *testing.T) {
	s := newTestStore(t)
	sentinel := errors.New("abort")
	err := s.WithBatch(func(b *store.Batch) error {
		_ = b.UpsertNode("c.go", "go_file", "package c", "h3")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if s.Exists("c.go") {
		t.Error("node should not exist after rollback")
	}
}

func TestStore_ClearFileContents_LIKEEscape(t *testing.T) {
	s := newTestStore(t)
	// file IDs that share a prefix differing only by the LIKE wildcard char '_'
	fileA := "pkg_a/file.go"
	fileB := "pkg/a_file.go" // starts with "pkg" but has '_' at a different position
	_ = s.UpsertNode(fileA, "go_file", "", "h1")
	_ = s.UpsertNode(fileA+":Foo", "function", "func Foo()", "")
	_ = s.UpsertNode(fileB, "go_file", "", "h2")
	_ = s.UpsertNode(fileB+":Bar", "function", "func Bar()", "")

	if err := s.ClearFileContents(fileA); err != nil {
		t.Fatalf("ClearFileContents: %v", err)
	}
	// fileA's symbol should be gone
	if s.Exists(fileA + ":Foo") {
		t.Error("symbol of cleared file should not exist")
	}
	// fileA node itself must still exist (cleared, not deleted)
	if !s.Exists(fileA) {
		t.Error("file node itself should survive ClearFileContents")
	}
	// fileB and its symbol must be untouched
	if !s.Exists(fileB) || !s.Exists(fileB+":Bar") {
		t.Error("unrelated file should not be affected by ClearFileContents")
	}
}

func TestStore_TraceEdges_Cycles(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertNode("a.go", "go_file", "", "h")
	_ = s.UpsertNode("b.go", "go_file", "", "h")
	_ = s.UpsertEdge("a.go", "b.go", "imports")
	_ = s.UpsertEdge("b.go", "a.go", "imports") // cycle

	edges, err := s.TraceEdges("a.go", 10)
	if err != nil {
		t.Fatalf("TraceEdges: %v", err)
	}
	// Should terminate without hitting the 10000 limit
	if len(edges) > 100 {
		t.Errorf("too many edges from cyclic graph: %d", len(edges))
	}
}

func TestStore_TraceEdges_DepthCap(t *testing.T) {
	s := newTestStore(t)
	// chain: a -> b -> c -> d
	for _, id := range []string{"a.go", "b.go", "c.go", "d.go"} {
		_ = s.UpsertNode(id, "go_file", "", "h")
	}
	_ = s.UpsertEdge("a.go", "b.go", "imports")
	_ = s.UpsertEdge("b.go", "c.go", "imports")
	_ = s.UpsertEdge("c.go", "d.go", "imports")

	edges, err := s.TraceEdges("a.go", 2)
	if err != nil {
		t.Fatalf("TraceEdges: %v", err)
	}
	// depth=2: a->b (depth 1), b->c (depth 2); c->d (depth 3) must not appear
	toIDs := map[string]bool{}
	for _, e := range edges {
		toIDs[e.ToID] = true
	}
	if toIDs["d.go"] {
		t.Error("depth cap not respected: d.go should not appear at depth 2")
	}
	if !toIDs["b.go"] || !toIDs["c.go"] {
		t.Error("expected b.go and c.go within depth 2")
	}
}

func TestStore_GetNodesIn_Chunking(t *testing.T) {
	s := newTestStore(t)
	const n = 1200
	ids := make([]string, n)
	err := s.WithBatch(func(b *store.Batch) error {
		for i := range ids {
			ids[i] = fmt.Sprintf("file%04d.go", i)
			if err := b.UpsertNode(ids[i], "go_file", "content", "h"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	got, err := s.GetNodesIn(ids)
	if err != nil {
		t.Fatalf("GetNodesIn: %v", err)
	}
	if len(got) != n {
		t.Errorf("expected %d nodes, got %d", n, len(got))
	}
}

func TestStore_GetNodesIn_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetNodesIn(nil)
	if err != nil {
		t.Fatalf("GetNodesIn(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestStore_FTSSearch_Sanitize(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertNode("x.go", "go_file", "hello world", "h")

	cases := []string{"", "  ", `foo"bar`, "hello world", `"quoted"`, "a b c"}
	for _, q := range cases {
		_, err := s.FTSSearch(q, 5)
		if err != nil {
			t.Errorf("FTSSearch(%q) returned error: %v", q, err)
		}
	}
}

func TestStore_GetSymbolCountsByDir(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertNode("pkg/a.go", "go_file", "", "h")
	_ = s.UpsertNode("pkg/a.go:Foo", "function", "func Foo()", "")
	_ = s.UpsertNode("pkg/a.go:Bar", "function", "func Bar()", "")
	_ = s.UpsertNode("other/b.go", "go_file", "", "h")
	_ = s.UpsertNode("other/b.go:Baz", "function", "func Baz()", "")

	counts, err := s.GetSymbolCountsByDir()
	if err != nil {
		t.Fatalf("GetSymbolCountsByDir: %v", err)
	}
	if counts["pkg"] != 2 {
		t.Errorf("expected 2 symbols in pkg, got %d", counts["pkg"])
	}
	if counts["other"] != 1 {
		t.Errorf("expected 1 symbol in other, got %d", counts["other"])
	}
}

func TestStore_Reset(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertNode("a.go", "go_file", "", "h")
	_ = s.UpsertEdge("a.go", "b.go", "imports")

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	nodes, edges, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if nodes != 0 || edges != 0 {
		t.Errorf("expected 0 nodes/edges after Reset, got %d/%d", nodes, edges)
	}
}

func TestStore_IsHashCurrent(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertNode("f.go", "go_file", "", "oldhash")
	if s.IsHashCurrent("f.go", "newhash") {
		t.Error("should not be current with different hash")
	}
	if !s.IsHashCurrent("f.go", "oldhash") {
		t.Error("should be current with same hash")
	}
}

func TestStore_TraceEdgesMulti_MultiSeed(t *testing.T) {
	s := newTestStore(t)
	// Graph: a -> b -> c, d -> b
	// Both a and d reach b at hop 1; a reaches c at hop 2.
	for _, id := range []string{"a.go", "b.go", "c.go", "d.go"} {
		_ = s.UpsertNode(id, "go_file", "", "h")
	}
	_ = s.UpsertEdge("a.go", "b.go", "imports")
	_ = s.UpsertEdge("b.go", "c.go", "imports")
	_ = s.UpsertEdge("d.go", "b.go", "imports")

	hops, edges, err := s.TraceEdgesMulti([]string{"a.go", "d.go"}, 2)
	if err != nil {
		t.Fatalf("TraceEdgesMulti: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("expected edges, got none")
	}
	// b.go must be reachable; both paths have hop=1, so MIN(depth)=1.
	if h, ok := hops["b.go"]; !ok {
		t.Error("b.go should be in hops map")
	} else if h != 1 {
		t.Errorf("b.go hop: want 1, got %d", h)
	}
	// c.go is 2 hops from a (a->b->c).
	if h, ok := hops["c.go"]; !ok {
		t.Error("c.go should be in hops map at depth 2")
	} else if h != 2 {
		t.Errorf("c.go hop: want 2, got %d", h)
	}
}

func TestStore_TraceEdgesMulti_EmptySeeds(t *testing.T) {
	s := newTestStore(t)
	hops, edges, err := s.TraceEdgesMulti([]string{}, 2)
	if err != nil {
		t.Fatalf("TraceEdgesMulti(empty): %v", err)
	}
	if len(hops) != 0 || len(edges) != 0 {
		t.Errorf("expected empty results for empty seeds, got hops=%d edges=%d", len(hops), len(edges))
	}
}

func TestStore_TraceEdgesMulti_DepthOne(t *testing.T) {
	s := newTestStore(t)
	// chain: a -> b -> c
	for _, id := range []string{"a.go", "b.go", "c.go"} {
		_ = s.UpsertNode(id, "go_file", "", "h")
	}
	_ = s.UpsertEdge("a.go", "b.go", "imports")
	_ = s.UpsertEdge("b.go", "c.go", "imports")

	hops, _, err := s.TraceEdgesMulti([]string{"a.go"}, 1)
	if err != nil {
		t.Fatalf("TraceEdgesMulti: %v", err)
	}
	if _, ok := hops["b.go"]; !ok {
		t.Error("b.go should be reachable at depth 1")
	}
	if _, ok := hops["c.go"]; ok {
		t.Error("c.go should NOT be reachable at depth 1")
	}
}

func TestStore_TraceNodeIDs(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"a.go", "b.go", "c.go"} {
		_ = s.UpsertNode(id, "go_file", "", "h")
	}
	_ = s.UpsertEdge("a.go", "b.go", "imports")
	_ = s.UpsertEdge("b.go", "c.go", "imports")

	ids, err := s.TraceNodeIDs("a.go", 2)
	if err != nil {
		t.Fatalf("TraceNodeIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"a.go", "b.go", "c.go"} {
		if !got[want] {
			t.Errorf("expected %q in TraceNodeIDs result, got %v", want, ids)
		}
	}
}

func TestStore_GetAllFileNodeIDs(t *testing.T) {
	s := newTestStore(t)
	// File nodes (type ends in _file)
	_ = s.UpsertNode("a.go", "go_file", "", "h")
	_ = s.UpsertNode("b.py", "py_file", "", "h")
	// Symbol and import nodes — must NOT appear in GetAllFileNodeIDs
	_ = s.UpsertNode("a.go:Foo", "function", "func Foo()", "")
	_ = s.UpsertNode("os", "import", "", "")

	ids, err := s.GetAllFileNodeIDs()
	if err != nil {
		t.Fatalf("GetAllFileNodeIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got["a.go"] {
		t.Error("a.go (go_file) should appear in GetAllFileNodeIDs")
	}
	if !got["b.py"] {
		t.Error("b.py (py_file) should appear in GetAllFileNodeIDs")
	}
	if got["a.go:Foo"] {
		t.Error("symbol node a.go:Foo should NOT appear in GetAllFileNodeIDs")
	}
	if got["os"] {
		t.Error("import node 'os' should NOT appear in GetAllFileNodeIDs")
	}
}
