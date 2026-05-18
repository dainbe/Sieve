package embed

import (
	"math"
)

// VectorIndex is an in-memory brute-force cosine similarity index.
// For corpora of ~10k symbols this takes <5ms per query without SIMD.
type VectorIndex struct {
	ids  []string
	vecs [][]float32 // parallel to ids; expected to be L2-normalized
}

// Hit is a single search result from VectorIndex.Search.
type Hit struct {
	ID    string
	Score float32 // cosine similarity [0, 1]
}

// NewVectorIndex builds an index from a map of node-id → embedding vector.
// Vectors are normalized on insertion; the input map is not modified.
func NewVectorIndex(vecs map[string][]float32) *VectorIndex {
	idx := &VectorIndex{
		ids:  make([]string, 0, len(vecs)),
		vecs: make([][]float32, 0, len(vecs)),
	}
	for id, v := range vecs {
		idx.ids = append(idx.ids, id)
		idx.vecs = append(idx.vecs, l2normalize(v))
	}
	return idx
}

// Len returns the number of vectors in the index.
func (idx *VectorIndex) Len() int { return len(idx.ids) }

// Search returns up to k nearest neighbors sorted by cosine similarity descending.
// The query vector is normalized internally; callers need not pre-normalize.
func (idx *VectorIndex) Search(query []float32, k int) []Hit {
	if len(query) == 0 || len(idx.ids) == 0 {
		return nil
	}
	query = l2normalize(query)
	type scored struct {
		i     int
		score float32
	}
	scores := make([]scored, len(idx.ids))
	for i, v := range idx.vecs {
		scores[i] = scored{i: i, score: dot(query, v)}
	}
	if k > len(scores) {
		k = len(scores)
	}
	// Partial selection sort for the top-k (efficient when k << n).
	for i := 0; i < k; i++ {
		maxIdx := i
		for j := i + 1; j < len(scores); j++ {
			if scores[j].score > scores[maxIdx].score {
				maxIdx = j
			}
		}
		scores[i], scores[maxIdx] = scores[maxIdx], scores[i]
	}
	hits := make([]Hit, k)
	for i := 0; i < k; i++ {
		hits[i] = Hit{ID: idx.ids[scores[i].i], Score: scores[i].score}
	}
	return hits
}

func dot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

func l2normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum < 1e-12 {
		return v
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

