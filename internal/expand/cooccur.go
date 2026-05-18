// Package expand implements model-free query expansion via PPMI co-occurrence.
// It reads the indexed corpus, computes document-level co-occurrence, and uses
// Positive Pointwise Mutual Information to find semantically related terms
// without external models or embeddings.
//
// Environment variables:
//
//	SIEVE_PPMI_DISABLE=1          Completely disables PPMI (BuildPPMI is a no-op).
//	SIEVE_PPMI_MIN_COUNT=N        Minimum co-occurrence count to keep a pair (default 2).
//	SIEVE_PPMI_REBUILD_THRESHOLD=N If fewer than N files changed since the last PPMI
//	                              build, skip the rebuild (default 100).
package expand

import (
	"log/slog"
	"math"
	"runtime"
	"sort"

	"github.com/dainbe/Sieve/internal/env"
	"github.com/dainbe/Sieve/internal/store"
)

const (
	minDocFreq      = 2   // discard terms that appear in fewer than this many documents
	maxTokensPerDoc = 300 // per-document token cap to prevent O(T²) pair explosion

	// pruneEvery is the number of documents processed before a sweep removes
	// co-occurrence entries below minCount. This bounds peak memory usage
	// without significantly affecting quality.
	pruneEvery = 500
)


// BuildPPMI computes PPMI co-occurrence neighbors for every term in the corpus
// and writes the results to the term_neighbors table in s.
//
// B.3: Memory control
//   - Pair entries with count < minCount are pruned whenever the map grows past
//     pruneEvery keys.
//   - After writing to the DB the in-memory co-occurrence map is set to nil so
//     the GC can reclaim it promptly.
//   - SIEVE_PPMI_DISABLE=1: early return, table is left unchanged.
//   - SIEVE_PPMI_MIN_COUNT (default 2): minimum co-occurrence count to retain a pair.
//
// B.4: Incremental rebuild
//   - changedFiles is the number of files changed during the most recent
//     IndexProject call.
//   - If the term_neighbors table is non-empty AND changedFiles <
//     SIEVE_PPMI_REBUILD_THRESHOLD (default 100), this function skips the
//     rebuild and returns nil immediately.
//
// tokenize should return vocabulary terms for a file's content. Deduplication
// within a single call is handled internally (co-occurrence is per-document).
func BuildPPMI(s *store.Store, tokenize func(string) []string, topK, changedFiles int) error {
	// B.3: early return if disabled.
	if env.Bool("SIEVE_PPMI_DISABLE") {
		slog.Debug("expand: PPMI disabled via SIEVE_PPMI_DISABLE; skipping")
		return nil
	}

	minCount := env.IntPos("SIEVE_PPMI_MIN_COUNT", 2)
	rebuildThreshold := env.IntPos("SIEVE_PPMI_REBUILD_THRESHOLD", 100)

	// B.4: skip rebuild if table is populated and few files changed.
	if changedFiles < rebuildThreshold {
		count, err := s.TermNeighborsCount()
		if err == nil && count > 0 {
			slog.Debug("expand: skipping PPMI rebuild",
				"changed_files", changedFiles,
				"threshold", rebuildThreshold,
				"existing_rows", count,
			)
			return nil
		}
	}

	nodes, err := s.GetAllFileNodes()
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	N := len(nodes)
	// docFreq[t] = number of documents containing t.
	docFreq := map[string]int{}
	// cooc[a][b] = number of documents containing both a and b (stored once: a < b).
	cooc := map[string]map[string]int{}
	addedSinceLastPrune := 0

	for _, n := range nodes {
		toks := tokenize(n.Content)
		if len(toks) == 0 {
			continue
		}
		// Deduplicate within this document so co-occurrence counts are per-document.
		seen := make(map[string]bool, len(toks))
		for _, t := range toks {
			seen[t] = true
		}
		for t := range seen {
			docFreq[t]++
		}
		// Build the unique terms list, capped to maxTokensPerDoc.
		terms := make([]string, 0, len(seen))
		for t := range seen {
			terms = append(terms, t)
		}
		if len(terms) > maxTokensPerDoc {
			terms = terms[:maxTokensPerDoc]
		}
		// Accumulate pairwise co-occurrence counts.
		for i, a := range terms {
			for j, b := range terms {
				if i >= j {
					continue
				}
				if cooc[a] == nil {
					cooc[a] = map[string]int{}
				}
				if cooc[b] == nil {
					cooc[b] = map[string]int{}
				}
				cooc[a][b]++
				cooc[b][a]++
			}
		}
		// B.3: prune low-count entries every pruneEvery documents.
		addedSinceLastPrune++
		if addedSinceLastPrune >= pruneEvery {
			pruneCooc(cooc, minCount)
			addedSinceLastPrune = 0
		}
	}

	// Final prune pass before scoring.
	pruneCooc(cooc, minCount)

	type scoredNeighbor struct {
		neighbor string
		weight   float64
	}

	Nf := float64(N)
	var result []store.TermNeighbor
	for term, neighbors := range cooc {
		if docFreq[term] < minDocFreq {
			continue
		}
		var scored []scoredNeighbor
		for nb, c := range neighbors {
			if c < minCount || docFreq[nb] < minDocFreq {
				continue
			}
			// PMI = log( P(a,b) / (P(a)*P(b)) )
			//     = log( cooc(a,b)*N / (docFreq(a)*docFreq(b)) )
			pmi := math.Log(float64(c) * Nf / (float64(docFreq[term]) * float64(docFreq[nb])))
			if pmi <= 0 {
				continue
			}
			scored = append(scored, scoredNeighbor{neighbor: nb, weight: pmi})
		}
		if len(scored) == 0 {
			continue
		}
		sort.Slice(scored, func(i, j int) bool {
			return scored[i].weight > scored[j].weight
		})
		k := topK
		if k > len(scored) {
			k = len(scored)
		}
		for _, s := range scored[:k] {
			result = append(result, store.TermNeighbor{
				Term:     term,
				Neighbor: s.neighbor,
				Weight:   s.weight,
			})
		}
	}

	// B.3: release the co-occurrence map before writing to DB so the GC can reclaim it.
	cooc = nil
	runtime.GC()

	slog.Debug("expand: PPMI build complete", "pairs", len(result))
	return s.ReplaceTermNeighbors(result)
}

// pruneCooc removes all pairs from cooc whose count is below minCount.
// This is called periodically during corpus traversal to bound peak memory.
func pruneCooc(cooc map[string]map[string]int, minCount int) {
	for a, neighbors := range cooc {
		for b, c := range neighbors {
			if c < minCount {
				delete(neighbors, b)
			}
		}
		if len(neighbors) == 0 {
			delete(cooc, a)
		}
	}
}

// BuildCooccurrence is the stateless batch version of PPMI computation.
// It is kept for callers that construct the neighbor list without a store.
// Returns up to topK neighbors per term, weight descending.
//
// tokenize should return the vocabulary terms for a file's content; it may
// deduplicate within a single call since co-occurrence is per-document.
func BuildCooccurrence(nodes []store.Node, tokenize func(string) []string, topK int) []store.TermNeighbor {
	N := len(nodes)
	if N == 0 {
		return nil
	}

	minCount := env.IntPos("SIEVE_PPMI_MIN_COUNT", 2)
	docFreq := map[string]int{}
	cooc := map[string]map[string]int{}
	addedSinceLastPrune := 0

	for _, n := range nodes {
		toks := tokenize(n.Content)
		if len(toks) == 0 {
			continue
		}
		seen := make(map[string]bool, len(toks))
		for _, t := range toks {
			seen[t] = true
		}
		for t := range seen {
			docFreq[t]++
		}
		terms := make([]string, 0, len(seen))
		for t := range seen {
			terms = append(terms, t)
		}
		if len(terms) > maxTokensPerDoc {
			terms = terms[:maxTokensPerDoc]
		}
		for i, a := range terms {
			for j, b := range terms {
				if i >= j {
					continue
				}
				if cooc[a] == nil {
					cooc[a] = map[string]int{}
				}
				if cooc[b] == nil {
					cooc[b] = map[string]int{}
				}
				cooc[a][b]++
				cooc[b][a]++
			}
		}
		addedSinceLastPrune++
		if addedSinceLastPrune >= pruneEvery {
			pruneCooc(cooc, minCount)
			addedSinceLastPrune = 0
		}
	}

	pruneCooc(cooc, minCount)

	type scoredNeighbor struct {
		neighbor string
		weight   float64
	}

	Nf := float64(N)
	var result []store.TermNeighbor
	for term, neighbors := range cooc {
		if docFreq[term] < minDocFreq {
			continue
		}
		var scored []scoredNeighbor
		for nb, c := range neighbors {
			if c < minCount || docFreq[nb] < minDocFreq {
				continue
			}
			pmi := math.Log(float64(c) * Nf / (float64(docFreq[term]) * float64(docFreq[nb])))
			if pmi <= 0 {
				continue
			}
			scored = append(scored, scoredNeighbor{neighbor: nb, weight: pmi})
		}
		if len(scored) == 0 {
			continue
		}
		sort.Slice(scored, func(i, j int) bool {
			return scored[i].weight > scored[j].weight
		})
		k := topK
		if k > len(scored) {
			k = len(scored)
		}
		for _, s := range scored[:k] {
			result = append(result, store.TermNeighbor{
				Term:     term,
				Neighbor: s.neighbor,
				Weight:   s.weight,
			})
		}
	}
	return result
}

// ExpandQuery returns additional query terms derived from neighbors of the input
// terms. It deduplicates against the original terms and keeps at most n neighbors
// per term (by weight, highest first). The original terms are NOT included in
// the returned slice.
//
// When SIEVE_PPMI_DISABLE=1 is set, ExpandQuery always returns nil.
func ExpandQuery(terms []string, getNeighbors func(string, int) ([]string, error), n int) []string {
	if env.Bool("SIEVE_PPMI_DISABLE") {
		return nil
	}
	origSet := make(map[string]bool, len(terms))
	for _, t := range terms {
		origSet[t] = true
	}
	seen := make(map[string]bool)
	var extra []string
	for _, t := range terms {
		nbs, err := getNeighbors(t, n)
		if err != nil {
			continue
		}
		for _, nb := range nbs {
			if origSet[nb] || seen[nb] {
				continue
			}
			seen[nb] = true
			extra = append(extra, nb)
		}
	}
	return extra
}
