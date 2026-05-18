package expand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dainbe/Sieve/internal/store"
)

// simpleTokenize splits on whitespace and lowercases — for tests only.
func simpleTokenize(content string) []string {
	fields := strings.Fields(strings.ToLower(content))
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func TestBuildCooccurrenceBasic(t *testing.T) {
	// Design Thoughts:
	// - Try 1: alpha+beta co-occur in 4 out of 6 docs; alpha alone or with gamma in 2.
	//   This makes P(alpha,beta) > P(alpha)*P(beta), producing positive PMI.
	//   alpha: 6, beta: 4, gamma: 2. alpha-beta cooc=4; PMI = log(4*6/(6*4)) = log(1) = 0 — still 0!
	// - Try 2: We need an asymmetric setup. Use 10 docs: beta appears in 5, alpha in 8,
	//   alpha-beta cooc=5. PMI = log(5*10/(8*5)) = log(10/8) = log(1.25) > 0.
	//   Now: alpha=13, delta=8, beta=5. alpha-delta cooc=8, alpha-beta cooc=5.
	//   PMI(alpha,delta) = log(8*13/(13*8)) = 0 again... all nodes have alpha.
	//
	// Final Design: completely separate clusters.
	// 5 docs: alpha+beta only; 3 docs: gamma+delta only; 2 docs: alpha+gamma (bridge).
	var nodes []store.Node

	for i := 0; i < 5; i++ {
		nodes = append(nodes, store.Node{ID: "ab" + string(rune('0'+i)) + ".go", Type: "go_file", Content: "alpha beta"})
	}
	for i := 0; i < 3; i++ {
		nodes = append(nodes, store.Node{ID: "gd" + string(rune('0'+i)) + ".go", Type: "go_file", Content: "gamma delta"})
	}
	for i := 0; i < 2; i++ {
		nodes = append(nodes, store.Node{ID: "ag" + string(rune('0'+i)) + ".go", Type: "go_file", Content: "alpha gamma"})
	}

	// N=10; alpha=7, beta=5, gamma=5, delta=3.
	// alpha-beta cooc=5; PMI = log(5*10/(7*5)) = log(10/7) ≈ 0.36 > 0 ✓
	// gamma-delta cooc=3; PMI = log(3*10/(5*3)) = log(2) ≈ 0.69 > 0 ✓
	// alpha-gamma cooc=2; PMI = log(2*10/(7*5)) = log(20/35) < 0 → filtered ✓
	pairs := BuildCooccurrence(nodes, simpleTokenize, 10)
	if len(pairs) == 0 {
		t.Fatal("expected non-empty pairs")
	}
	// alpha-beta pair must be present.
	found := false
	for _, p := range pairs {
		if (p.Term == "alpha" && p.Neighbor == "beta") || (p.Term == "beta" && p.Neighbor == "alpha") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected alpha-beta pair in result, got %+v", pairs)
	}
	// All weights must be positive.
	for _, p := range pairs {
		if p.Weight <= 0 {
			t.Errorf("non-positive weight for pair %+v", p)
		}
	}
}

func TestBuildCooccurrenceEmpty(t *testing.T) {
	if got := BuildCooccurrence(nil, simpleTokenize, 5); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestExpandQueryDisabled(t *testing.T) {
	t.Setenv("SIEVE_PPMI_DISABLE", "1")
	called := false
	got := ExpandQuery([]string{"foo"}, func(string, int) ([]string, error) {
		called = true
		return []string{"bar"}, nil
	}, 3)
	if called {
		t.Error("getNeighbors should not be called when PPMI is disabled")
	}
	if got != nil {
		t.Errorf("expected nil when disabled, got %v", got)
	}
}

func TestExpandQuery(t *testing.T) {
	os.Unsetenv("SIEVE_PPMI_DISABLE")
	got := ExpandQuery([]string{"foo"}, func(term string, n int) ([]string, error) {
		if term == "foo" {
			return []string{"bar", "baz"}, nil
		}
		return nil, nil
	}, 3)
	if len(got) != 2 {
		t.Errorf("expected 2 results, got %d: %v", len(got), got)
	}
}

func TestExpandQueryDedup(t *testing.T) {
	os.Unsetenv("SIEVE_PPMI_DISABLE")
	got := ExpandQuery([]string{"foo", "bar"}, func(term string, n int) ([]string, error) {
		// Both foo and bar return "baz" as neighbor.
		return []string{"baz"}, nil
	}, 3)
	count := 0
	for _, g := range got {
		if g == "baz" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected baz to appear exactly once, got %d times", count)
	}
}

func TestBuildPPMIDisabled(t *testing.T) {
	t.Setenv("SIEVE_PPMI_DISABLE", "1")
	// Create an in-memory store.
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	// Insert a file node so there's data to process.
	if err := s.UpsertNode("x.go", "go_file", "foo bar foo bar", "h1"); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	if err := BuildPPMI(s, simpleTokenize, 5, 200); err != nil {
		t.Fatalf("BuildPPMI error: %v", err)
	}
	// Table should remain empty because PPMI is disabled.
	count, err := s.TermNeighborsCount()
	if err != nil {
		t.Fatalf("TermNeighborsCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows when disabled, got %d", count)
	}
}

func TestBuildPPMIBasic(t *testing.T) {
	os.Unsetenv("SIEVE_PPMI_DISABLE")
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	// Use the same asymmetric cluster design as TestBuildCooccurrenceBasic:
	// 5 docs with alpha+beta, 3 docs with gamma+delta, 2 docs with alpha+gamma.
	// N=10; alpha-beta PMI = log(10/7) > 0 ✓
	docs := []struct{ id, content string }{}
	for i := 0; i < 5; i++ {
		docs = append(docs, struct{ id, content string }{"ab" + string(rune('0'+i)) + ".go", "alpha beta"})
	}
	for i := 0; i < 3; i++ {
		docs = append(docs, struct{ id, content string }{"gd" + string(rune('0'+i)) + ".go", "gamma delta"})
	}
	for i := 0; i < 2; i++ {
		docs = append(docs, struct{ id, content string }{"ag" + string(rune('0'+i)) + ".go", "alpha gamma"})
	}
	for _, d := range docs {
		if err := s.UpsertNode(d.id, "go_file", d.content, ""); err != nil {
			t.Fatalf("UpsertNode: %v", err)
		}
	}

	if err := BuildPPMI(s, simpleTokenize, 5, 200); err != nil {
		t.Fatalf("BuildPPMI: %v", err)
	}
	count, err := s.TermNeighborsCount()
	if err != nil {
		t.Fatalf("TermNeighborsCount: %v", err)
	}
	if count == 0 {
		t.Error("expected term_neighbors rows after BuildPPMI")
	}

	// alpha and beta should be neighbors.
	nbs, err := s.GetTermNeighbors("alpha", 10)
	if err != nil {
		t.Fatalf("GetTermNeighbors: %v", err)
	}
	found := false
	for _, nb := range nbs {
		if nb == "beta" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected beta as neighbor of alpha, got %v", nbs)
	}
}

func TestBuildPPMISkipThreshold(t *testing.T) {
	os.Unsetenv("SIEVE_PPMI_DISABLE")
	t.Setenv("SIEVE_PPMI_REBUILD_THRESHOLD", "100")

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	// Pre-populate term_neighbors with a sentinel pair.
	sentinel := []store.TermNeighbor{{Term: "sentinel", Neighbor: "value", Weight: 1.0}}
	if err := s.ReplaceTermNeighbors(sentinel); err != nil {
		t.Fatalf("ReplaceTermNeighbors: %v", err)
	}

	// Add a file node.
	if err := s.UpsertNode("a.go", "go_file", "foo bar foo bar", "h1"); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	// changedFiles=5 < threshold=100 and table is non-empty: should skip rebuild.
	if err := BuildPPMI(s, simpleTokenize, 5, 5); err != nil {
		t.Fatalf("BuildPPMI: %v", err)
	}

	// Sentinel should still be there.
	nbs, err := s.GetTermNeighbors("sentinel", 1)
	if err != nil {
		t.Fatalf("GetTermNeighbors: %v", err)
	}
	if len(nbs) == 0 || nbs[0] != "value" {
		t.Errorf("expected sentinel pair to remain, got %v", nbs)
	}
}

func TestBuildPPMIMinCount(t *testing.T) {
	os.Unsetenv("SIEVE_PPMI_DISABLE")
	t.Setenv("SIEVE_PPMI_MIN_COUNT", "3")

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	// Design: alpha+beta co-occur in 2 docs; alpha+baz co-occur in 4 docs.
	// N=10 docs total (need enough for PMI > 0 after the minCount filter).
	// alpha: 10, beta: 2, baz: 4.
	// PMI(alpha,baz) = log(4*10/(10*4)) = 0. Still 0 with full overlap.
	//
	// Use isolation: 4 baz-only docs, 4 alpha+baz docs, 2 alpha+beta docs.
	// Then alpha=6, baz=8, beta=2, N=10.
	// alpha-baz cooc=4; PMI = log(4*10/(6*8)) = log(40/48) < 0. Not great.
	//
	// Better isolation: alpha never appears without baz OR beta; baz/beta clusters.
	// 6 docs with alpha+baz, 2 docs with alpha+beta, 2 docs with baz+gamma.
	// N=10; alpha=8, baz=8, beta=2, gamma=2.
	// alpha-baz cooc=6; PMI = log(6*10/(8*8)) = log(60/64) ≈ -0.065 < 0. Barely negative.
	//
	// Simplest: use non-overlapping clusters + bridge.
	// 4 docs: alpha+baz, 1 doc: alpha+beta, 1 doc: alpha+beta, 4 docs: gamma+delta.
	// N=10; alpha=6, baz=4, beta=2, gamma=4, delta=4.
	// alpha-baz cooc=4; PMI = log(4*10/(6*4)) = log(40/24) ≈ 0.51 > 0 ✓
	// alpha-beta cooc=2; PMI = log(2*10/(6*2)) = log(20/12) ≈ 0.51 > 0 ✓
	// BUT minCount=3 filters alpha-beta (cooc=2 < 3) ✓ and keeps alpha-baz (cooc=4 ≥ 3) ✓
	docs := []struct{ id, content string }{}
	for i := 0; i < 4; i++ {
		docs = append(docs, struct{ id, content string }{"az" + string(rune('0'+i)) + ".go", "alpha baz"})
	}
	for i := 0; i < 2; i++ {
		docs = append(docs, struct{ id, content string }{"ab" + string(rune('0'+i)) + ".go", "alpha beta"})
	}
	for i := 0; i < 4; i++ {
		docs = append(docs, struct{ id, content string }{"gd" + string(rune('0'+i)) + ".go", "gamma delta"})
	}
	for _, d := range docs {
		if err := s.UpsertNode(d.id, "go_file", d.content, ""); err != nil {
			t.Fatalf("UpsertNode: %v", err)
		}
	}

	if err := BuildPPMI(s, simpleTokenize, 5, 200); err != nil {
		t.Fatalf("BuildPPMI: %v", err)
	}

	nbs, err := s.GetTermNeighbors("alpha", 10)
	if err != nil {
		t.Fatalf("GetTermNeighbors: %v", err)
	}
	// alpha+beta (cooc=2) should NOT be present (minCount=3).
	for _, nb := range nbs {
		if nb == "beta" {
			t.Error("alpha-beta pair should be absent (below minCount=3)")
		}
	}
	// alpha+baz (cooc=4) should be present.
	found := false
	for _, nb := range nbs {
		if nb == "baz" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected baz as neighbor of alpha (cooc=4 >= minCount=3), got %v", nbs)
	}
}
