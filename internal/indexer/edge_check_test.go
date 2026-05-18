//go:build eval

package indexer

import (
	"context"
	"strings"
	"testing"
)

func TestEdgeToHeuristic(t *testing.T) {
	_, s := setupTest(t)
	ctx := context.Background()

	_, err := IndexProject(ctx, s, nil, "", "../..")
	if err != nil {
		t.Fatalf("IndexProject: %v", err)
	}

	edges, err := s.TraceEdges("internal/indexer/indexer.go", 3)
	if err != nil {
		t.Fatalf("TraceEdges: %v", err)
	}
	t.Logf("Total edges from indexer.go (depth<=3): %d", len(edges))

	var heuristicEdges, extractAndStoreEdges []string
	for _, e := range edges {
		if strings.Contains(e.ToID, "heuristic") {
			heuristicEdges = append(heuristicEdges, e.FromID+" -["+e.Relation+"]-> "+e.ToID)
		}
		if e.FromID == "internal/indexer/indexer.go:extractAndStoreSymbols" {
			extractAndStoreEdges = append(extractAndStoreEdges, "-["+e.Relation+"]-> "+e.ToID)
		}
	}

	t.Logf("Edges FROM extractAndStoreSymbols:")
	for _, e := range extractAndStoreEdges {
		t.Logf("  %s", e)
	}
	t.Logf("Edges TO heuristic.go:")
	for _, e := range heuristicEdges {
		t.Logf("  %s", e)
	}

	if len(heuristicEdges) == 0 {
		t.Error("no edges to heuristic.go found — call edge missing")
	}
}
