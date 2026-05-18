// Package context implements Sieve's core context-building logic.
package context

import (
	goctx "context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dainbe/Sieve/internal/embed"
	"github.com/dainbe/Sieve/internal/env"
	"github.com/dainbe/Sieve/internal/expand"
	"github.com/dainbe/Sieve/internal/store"
)

// Defaults — overridable via environment variables for tuning.
var (
	maxTokens    = env.Int("SIEVE_MAX_TOKENS", 4000)
	graphDepth   = env.Int("SIEVE_GRAPH_DEPTH", 3)
	ftsFileLimit = env.Int("SIEVE_FTS_FILE_LIMIT", 200)
	// queryExpansionN controls PPMI query expansion: neighbors per query term.
	// Disabled by default (0) because the document-level co-occurrence signal
	// is too noisy on small corpora (< ~200 files). Enable by setting
	// SIEVE_QUERY_EXPANSION=3 (or higher) once the corpus is large enough.
	queryExpansionN = env.Int("SIEVE_QUERY_EXPANSION", 0)

	// denseK: how many file hits to request from the vector index.
	denseK = env.Int("SIEVE_DENSE_K", 10)

	// denseFraction: dense bonus = cosine × topFTSScore × denseFraction.
	// Keeps the dense signal proportional to FTS quality so it augments
	// rather than overrides keyword ranking. Tune with SIEVE_DENSE_FRACTION.
	denseFraction = env.Float("SIEVE_DENSE_FRACTION", 0.25)

	// graphSeedTopK caps the number of FTS/dense seeds passed to TraceEdgesMulti.
	// Keeping only the top-K seeds prevents O(seeds × depth) BFS explosion on
	// large projects while preserving precision: the highest-scoring files drive
	// the graph expansion.
	graphSeedTopK = env.Int("SIEVE_GRAPH_SEED_TOP_K", 30)

	// scoreThreshold: relative cutoff — nodes below (topComposite × threshold)
	// are dropped. Filters graph noise while keeping high-signal FTS hits.
	scoreThreshold = env.Float("SIEVE_SCORE_THRESHOLD", 0.25)
)

const charsPerToken = 4

// candidate is the internal scoring unit for the Build pipeline.
type candidate struct {
	node       store.Node
	score      float64
	source     string // "fts" | "dense" | "graph"
	hops       int
	graphBoost float64
}

// ContextNode is a single entry in the built context.
type ContextNode struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"`
	Content string  `json:"content"` // compressed or summary-only in stage 1
	Score   float64 `json:"score"`
	Source  string  `json:"source"` // "fts" | "graph"
}

// Branch describes an explorable subtree visible from the current result.
// Corresponds to Corpus2Skill's "bird's-eye view" of adjacent corpus regions.
type Branch struct {
	Path        string `json:"path"`
	FileCount   int    `json:"file_count"`
	SymbolCount int    `json:"symbol_count"`
	Summary     string `json:"summary"` // what this directory contains (PageIndex-style node description)
	Hint        string `json:"hint"`    // uncovered file count
}

// Result is what ctx_build_context returns to the AI.
type Result struct {
	Nodes         []ContextNode `json:"nodes"`
	Branches      []Branch      `json:"branches"` // explorable adjacent subtrees (drill down here if context is insufficient)
	TokenEstimate int           `json:"token_estimate"`
	Truncated     bool          `json:"truncated"`
	Insufficient  bool          `json:"insufficient,omitempty"`   // true when context may be incomplete; consider ctx_drill_down
	SuggestedNext []string      `json:"suggested_next,omitempty"` // branch paths most likely to contain relevant context
	Message       string        `json:"message,omitempty"`
}

// Builder assembles context from the store.
type Builder struct {
	store  *store.Store
	emb    *embed.Embedder    // nil when dense retrieval is disabled
	vecIdx *embed.VectorIndex // nil when dense retrieval is disabled or no vectors
}

func NewBuilder(s *store.Store) *Builder {
	return &Builder{store: s}
}

// SetEmbedder attaches a dense retrieval backend to the builder.
// Call after NewBuilder when SIEVE_DENSE_RETRIEVAL != "0".
func (b *Builder) SetEmbedder(e *embed.Embedder, idx *embed.VectorIndex) {
	b.emb = e
	b.vecIdx = idx
}

// Build returns a stage-1 result: compressed summaries + branch map.
// The agent can call DrillDown on any branch for full content.
//
// Pipeline: query expansion → FTS scoring → dense fusion → graph expansion
// → score threshold → token-budget fill.
func (b *Builder) Build(query string) (Result, error) {
	qterms := store.TokenizeFTS(query)
	ftsQuery := b.buildFTSQuery(query, qterms)

	ftsHits, err := b.store.FTSSearchFiles(ftsQuery, ftsFileLimit)
	if err != nil {
		return Result{}, fmt.Errorf("fts search: %w", err)
	}
	if len(ftsHits) == 0 {
		return Result{
			Message: "No indexed content matched the query. Run ctx_index_project first.",
		}, nil
	}

	byID := scoreFTSHits(ftsHits, qterms)
	b.fuseDense(query, byID)
	b.expandGraph(byID)

	sorted := rankCandidates(byID)
	nodes, used, truncated := fillBudget(sorted)

	includedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		includedIDs[n.ID] = true
	}
	branches := b.computeBranches(includedIDs)

	return Result{
		Nodes:         nodes,
		Branches:      branches,
		TokenEstimate: used,
		Truncated:     truncated,
		Insufficient:  truncated || len(branches) > 0,
		SuggestedNext: topBranchPaths(branches, 2),
	}, nil
}

// buildFTSQuery optionally widens the query with PPMI co-occurrence neighbors.
// qterms is derived from the original query (not the expanded one) so that
// termCoverage scoring stays anchored to what the user actually asked for.
func (b *Builder) buildFTSQuery(query string, qterms []string) string {
	if queryExpansionN == 0 || len(qterms) == 0 {
		return query
	}
	extra := expand.ExpandQuery(qterms, func(term string, n int) ([]string, error) {
		return b.store.GetTermNeighbors(term, n)
	}, queryExpansionN)
	if len(extra) > 0 {
		return query + " " + strings.Join(extra, " ")
	}
	return query
}

// scoreFTSHits converts raw FTS results into candidates using
// min-max normalised BM25 × typeFactor × termCoverage.
func scoreFTSHits(hits []store.Node, qterms []string) map[string]*candidate {
	var minBM25, maxBM25 float64
	for i, n := range hits {
		if i == 0 {
			minBM25, maxBM25 = n.Score, n.Score
		} else {
			if n.Score < minBM25 {
				minBM25 = n.Score
			}
			if n.Score > maxBM25 {
				maxBM25 = n.Score
			}
		}
	}
	bm25Range := maxBM25 - minBM25

	byID := make(map[string]*candidate, len(hits))
	for _, n := range hits {
		var norm float64
		if bm25Range > 1e-9 {
			norm = (n.Score - minBM25) / bm25Range
		} else {
			norm = 1.0
		}
		factor := ftsTypeFactor(n.Type, n.ID)
		cov := termCoverage(n.Content, n.ID, qterms)
		byID[n.ID] = &candidate{
			node:   n,
			score:  (0.5 + norm) * factor * (0.2 + 0.8*cov),
			source: "fts",
		}
	}
	return byID
}

// fuseDense adds cosine-similarity bonuses from the vector index.
// Existing FTS candidates are boosted; new dense-only files are seeded.
// Bonus = cosine × topFTSScore × denseFraction, capping dense influence
// at ~denseFraction of the strongest FTS hit.
// No-ops when SIEVE_DENSE_RETRIEVAL=0 or the embedder is not configured.
func (b *Builder) fuseDense(query string, byID map[string]*candidate) {
	if os.Getenv("SIEVE_DENSE_RETRIEVAL") == "0" {
		return
	}
	if b.emb == nil || b.vecIdx == nil || b.vecIdx.Len() == 0 {
		return
	}
	qvec, err := b.emb.EmbedOne(goctx.Background(), query)
	if err != nil {
		return
	}
	hits := b.vecIdx.Search(qvec, denseK)

	var topFTSScore float64
	for _, c := range byID {
		if c.source == "fts" && c.score > topFTSScore {
			topFTSScore = c.score
		}
	}
	if topFTSScore == 0 {
		topFTSScore = 1.0 // fallback when FTS returned no hits
	}

	var denseOnlyIDs []string
	for _, h := range hits {
		fid := fileIDFromNode(h.ID)
		bonus := float64(h.Score) * topFTSScore * denseFraction
		if c, ok := byID[fid]; ok {
			c.score += bonus
		} else {
			byID[fid] = &candidate{score: bonus, source: "dense"}
			denseOnlyIDs = append(denseOnlyIDs, fid)
		}
	}
	if len(denseOnlyIDs) > 0 {
		fetched, fetchErr := b.store.GetNodesIn(denseOnlyIDs)
		if fetchErr == nil {
			for id, n := range fetched {
				if c, ok := byID[id]; ok {
					c.node = n
				}
			}
		}
	}
}

// expandGraph runs BFS from the top-scoring seeds and merges discovered
// nodes into byID with hop-distance-weighted scores.
func (b *Builder) expandGraph(byID map[string]*candidate) {
	type seedEntry struct {
		id    string
		score float64
	}
	seeds := make([]seedEntry, 0, len(byID))
	for id, c := range byID {
		seeds = append(seeds, seedEntry{id, c.score})
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].score > seeds[j].score })
	k := graphSeedTopK
	if k > len(seeds) {
		k = len(seeds)
	}
	seedIDs := make([]string, k)
	for i := range seedIDs {
		seedIDs[i] = seeds[i].id
	}

	hopMap, _, err := b.store.TraceEdgesMulti(seedIDs, graphDepth)
	if err != nil {
		return
	}

	var pendingSymIDs []string
	pendingFileByID := map[string]int{}
	for toID, hops := range hopMap {
		boost := 0.15 / math.Log(float64(hops+2))
		if c, seen := byID[toID]; seen {
			c.score += boost
			continue
		}
		fid := fileIDFromNode(toID)
		if fid == toID {
			// pure file node not yet seen
			if h, ok := pendingFileByID[fid]; !ok || hops < h {
				pendingFileByID[fid] = hops
			}
			continue
		}
		// symbol node: boost parent file if already seen
		if c, seen := byID[fid]; seen {
			if boost > c.graphBoost {
				c.score += boost - c.graphBoost
				c.graphBoost = boost
			}
			pendingSymIDs = append(pendingSymIDs, toID)
		} else {
			if h, ok := pendingFileByID[fid]; !ok || hops < h {
				pendingFileByID[fid] = hops
			}
			pendingSymIDs = append(pendingSymIDs, toID)
		}
	}

	if len(pendingSymIDs) > 0 {
		fetched, fetchErr := b.store.GetNodesIn(pendingSymIDs)
		if fetchErr == nil {
			for id, n := range fetched {
				hops := hopMap[id]
				byID[id] = &candidate{
					node:   n,
					score:  0.3 / math.Log(float64(hops+2)),
					source: "graph",
					hops:   hops,
				}
			}
		}
	}
	if len(pendingFileByID) > 0 {
		pendingIDs := make([]string, 0, len(pendingFileByID))
		for id := range pendingFileByID {
			pendingIDs = append(pendingIDs, id)
		}
		fetched, fetchErr := b.store.GetNodesIn(pendingIDs)
		if fetchErr == nil {
			for id, n := range fetched {
				hops := pendingFileByID[id]
				byID[id] = &candidate{
					node:   n,
					score:  0.3 / math.Log(float64(hops+2)),
					source: "graph",
					hops:   hops,
				}
			}
		}
	}
}

// rankCandidates sorts candidates by composite score (score + typeBoost) descending.
func rankCandidates(byID map[string]*candidate) []*candidate {
	out := make([]*candidate, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].score+typeBoost(out[i].node.Type) >
			out[j].score+typeBoost(out[j].node.Type)
	})
	return out
}

// fillBudget applies the score threshold cutoff and fills the token budget
// with compressed node content, falling back to harder compression on overflow.
func fillBudget(sorted []*candidate) (nodes []ContextNode, used int, truncated bool) {
	var topComposite float64
	if len(sorted) > 0 {
		topComposite = sorted[0].score + typeBoost(sorted[0].node.Type)
	}
	cutoff := topComposite * scoreThreshold

	for _, c := range sorted {
		if c.node.ID == "" {
			continue // fetch failed during graph expansion
		}
		if scoreThreshold > 0 && c.score+typeBoost(c.node.Type) < cutoff {
			truncated = true
			continue
		}
		compressed := compress(c.node)
		tokens := estimateTokens(compressed)
		if used+tokens > maxTokens {
			compressed = compressHard(c.node)
			tokens = estimateTokens(compressed)
			if used+tokens > maxTokens {
				truncated = true
				continue
			}
		}
		nodes = append(nodes, ContextNode{
			ID:      c.node.ID,
			Type:    c.node.Type,
			Content: compressed,
			Score:   math.Round(c.score*1e4) / 1e4,
			Source:  c.source,
		})
		used += tokens
	}
	return
}

// DrillDown returns full (uncompressed) content for all nodes under path.
// Corresponds to Corpus2Skill's agent "drilling into a topic branch."
func (b *Builder) DrillDown(path string) (Result, error) {
	matched, err := b.store.GetFileNodesByPrefix(path)
	if err != nil {
		return Result{}, fmt.Errorf("list nodes: %w", err)
	}

	fetched := make(map[string]store.Node, len(matched))
	matchedIDs := make([]string, 0, len(matched))
	for _, n := range matched {
		fetched[n.ID] = n
		matchedIDs = append(matchedIDs, n.ID)
	}

	var nodes []ContextNode
	used := 0
	anySkipped := false

	for _, id := range matchedIDs {
		n, ok := fetched[id]
		if !ok {
			continue
		}
		// DrillDown returns fuller content than Build
		content := compressFile(n.Content, n.Type, 120)
		tokens := estimateTokens(content)
		if used+tokens > maxTokens {
			anySkipped = true
			continue
		}
		nodes = append(nodes, ContextNode{
			ID:      n.ID,
			Type:    n.Type,
			Content: content,
			Score:   1.0,
			Source:  "drill",
		})
		used += tokens
	}

	if len(nodes) == 0 {
		return Result{
			Message: fmt.Sprintf("No indexed nodes found under %q. Check the path or run ctx_index_project.", path),
		}, nil
	}

	// Re-compute branches from drill-down scope for continued navigation
	includedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		includedIDs[n.ID] = true
	}
	branches := b.computeBranches(includedIDs)

	return Result{
		Nodes:         nodes,
		Branches:      branches,
		TokenEstimate: used,
		Truncated:     anySkipped,
		Insufficient:  anySkipped || len(branches) > 0,
		SuggestedNext: topBranchPaths(branches, 2),
	}, nil
}

// topBranchPaths returns the paths of the first n branches.
func topBranchPaths(branches []Branch, n int) []string {
	var out []string
	for i, br := range branches {
		if i >= n {
			break
		}
		out = append(out, br.Path)
	}
	return out
}

// fileIDFromNode strips a trailing ":SymbolName" suffix from a graph node ID,
// returning the bare file path. Graph traversal may yield symbol IDs; this
// normalises them to the file level for candidate map merging.
func fileIDFromNode(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[:i]
	}
	return id
}

// ftsTypeFactor demotes non-code files and test files so they don't displace
// production code in relevance ranking for code-centric queries.
func ftsTypeFactor(nodeType, nodeID string) float64 {
	// Test files have high BM25 density (short, term-rich) but rarely represent
	// the primary implementation of a feature — demote them uniformly.
	if strings.HasSuffix(nodeID, "_test.go") {
		return 0.5
	}
	switch nodeType {
	case "go_file", "ts_file", "js_file", "py_file", "rs_file":
		return 1.0
	case "text_file":
		return 0.3
	default:
		return 0.7
	}
}

// termCoverage returns the fraction of query terms that appear as tokens in
// the node's FTS vocabulary (content + ID). Uses store.TokenizeFTS for
// word-boundary matching so short tokens like "go" don't spuriously match
// camelCase substrings ("google", "golang" etc.).
func termCoverage(content, id string, terms []string) float64 {
	if len(terms) == 0 {
		return 1.0
	}
	tokSet := make(map[string]bool)
	for _, t := range store.TokenizeFTS(content) {
		tokSet[t] = true
	}
	for _, t := range store.TokenizeFTS(id) {
		tokSet[t] = true
	}
	count := 0
	for _, t := range terms {
		if tokSet[t] {
			count++
		}
	}
	return float64(count) / float64(len(terms))
}

func typeBoost(nodeType string) float64 {
	switch nodeType {
	case "function", "method", "type", "variable", "class", "struct",
		"interface", "type_alias", "constant", "trait", "impl", "enum":
		return 0.5
	case "go_file", "ts_file", "js_file", "py_file", "rs_file":
		return 0.2
	case "import":
		return 0.0
	default:
		return 0.1
	}
}

func compress(n store.Node) string {
	switch n.Type {
	case "function", "method", "type", "variable", "class", "struct",
		"interface", "type_alias", "constant", "trait", "impl", "enum":
		if n.Content != "" {
			lines := strings.Split(n.Content, "\n")
			if len(lines) > 15 {
				return strings.Join(lines[:15], "\n") + "\n... (signature truncated)"
			}
			return n.Content
		}
		return n.ID
	case "import":
		return n.ID
	case "go_file", "ts_file", "js_file", "py_file", "rs_file":
		return compressFile(n.Content, n.Type, 60)
	default:
		return truncateLines(n.Content, 30)
	}
}

func compressHard(n store.Node) string {
	switch n.Type {
	case "function", "method", "type", "variable", "class", "struct",
		"interface", "type_alias", "constant", "trait", "impl", "enum":
		if n.Content != "" {
			return truncateLines(n.Content, 3)
		}
		return n.ID
	case "import":
		return n.ID
	default:
		return truncateLines(n.Content, 8)
	}
}

func compressFile(content, nodeType string, maxLines int) string {
	switch nodeType {
	case "go_file":
		return compressGoFile(content, maxLines)
	case "py_file":
		return compressPyFile(content, maxLines)
	default:
		return truncateLines(content, maxLines)
	}
}

// compressGoFile extracts top-level signatures from Go source using go/parser.
// This replaces the old brace-counting heuristic that mishandled string literals.
func compressGoFile(content string, maxLines int) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, parser.ParseComments)
	if err != nil {
		// Fall back to a safe line count on unparseable input.
		return truncateLines(content, maxLines)
	}

	srcLines := strings.Split(content, "\n")
	lineRange := func(start, end token.Pos) string {
		s := fset.Position(start).Line - 1
		e := fset.Position(end).Line - 1
		if s < 0 || s >= len(srcLines) {
			return ""
		}
		if s == e {
			return strings.TrimRight(srcLines[s], " \t")
		}
		// Multi-line: collect up to the opening brace for functions.
		var sb strings.Builder
		for i := s; i <= e && i < len(srcLines); i++ {
			trimmed := strings.TrimRight(srcLines[i], " \t")
			sb.WriteString(trimmed)
			if strings.HasSuffix(strings.TrimSpace(trimmed), "{") {
				break
			}
			sb.WriteByte('\n')
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	var out []string
	// Package declaration
	if f.Name != nil {
		pkgLine := fset.Position(f.Package).Line - 1
		if pkgLine >= 0 && pkgLine < len(srcLines) {
			out = append(out, srcLines[pkgLine])
		}
	}
	// Import block (include full block so import paths are visible)
	for _, imp := range f.Imports {
		if imp.Path != nil {
			// Deduplicated: just emit the path value once
			out = append(out, strings.Trim(imp.Path.Value, `"`))
		}
	}
	// Top-level declarations: signatures only (no bodies)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				sig := lineRange(d.Pos(), d.Body.Lbrace)
				if sig != "" {
					out = append(out, sig)
				}
			}
		case *ast.GenDecl:
			// type / var / const
			if d.Tok.String() == "import" {
				continue // already handled above
			}
			sig := lineRange(d.Pos(), d.End())
			if sig != "" {
				out = append(out, sig)
			}
		}
	}

	if len(out) == 0 {
		return truncateLines(content, maxLines)
	}
	return truncateLines(strings.Join(out, "\n"), maxLines)
}

// compressPyFile extracts import/def/class signature lines from Python source.
func compressPyFile(content string, maxLines int) string {
	lines := strings.Split(content, "\n")
	var out []string
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "import ") || strings.HasPrefix(s, "from ") ||
			strings.HasPrefix(s, "def ") || strings.HasPrefix(s, "class ") ||
			strings.HasPrefix(s, "async def ") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return truncateLines(content, maxLines)
	}
	return truncateLines(strings.Join(out, "\n"), maxLines)
}

func truncateLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n") +
		fmt.Sprintf("\n... (%d lines omitted)", len(lines)-n)
}

func estimateTokens(text string) int {
	n := len(text) / charsPerToken
	if n == 0 {
		return 1
	}
	return n
}

func FileContext(content string, targetLine, radius int) string {
	lines := strings.Split(content, "\n")
	start := targetLine - radius - 1
	end := targetLine + radius
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	var out []string
	for i := start; i < end; i++ {
		prefix := "  "
		if i == targetLine-1 {
			prefix = "> "
		}
		out = append(out, fmt.Sprintf("%s%4d: %s", prefix, i+1, lines[i]))
	}
	return strings.Join(out, "\n")
}

func SummaryLine(n store.Node) string {
	switch n.Type {
	case "function", "type", "variable":
		sig := n.Content
		if sig == "" {
			sig = n.ID
		}
		first := strings.SplitN(sig, "\n", 2)[0]
		return fmt.Sprintf("[%s] %s — %s", n.Type, n.ID, strings.TrimSpace(first))
	case "import":
		return fmt.Sprintf("[import] %s", n.ID)
	default:
		ext := filepath.Ext(n.ID)
		lineCount := strings.Count(n.Content, "\n") + 1
		return fmt.Sprintf("[%s] %s (%d lines)", strings.TrimSuffix(n.Type, "_file")+ext, n.ID, lineCount)
	}
}
