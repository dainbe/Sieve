// Package context implements Sieve's core context-building logic.
package context

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dainbe/Sieve/internal/store"
)

// Defaults — overridable via environment variables for tuning.
var (
	maxTokens  = envInt("SIEVE_MAX_TOKENS", 4000)
	graphDepth = envInt("SIEVE_GRAPH_DEPTH", 2)
	ftsLimit   = envInt("SIEVE_FTS_LIMIT", 10)
)

const charsPerToken = 4

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
	store *store.Store
}

func NewBuilder(s *store.Store) *Builder {
	return &Builder{store: s}
}

// Build returns a stage-1 result: compressed summaries + branch map.
// The agent can call DrillDown on any branch for full content.
func (b *Builder) Build(query string) (Result, error) {
	ftsHits, err := b.store.FTSSearch(query, ftsLimit)
	if err != nil {
		return Result{}, fmt.Errorf("fts search: %w", err)
	}
	if len(ftsHits) == 0 {
		return Result{
			Message: "No indexed content matched the query. Run ctx_index_project first.",
		}, nil
	}

	type candidate struct {
		node   store.Node
		score  float64
		source string
		hops   int
	}
	byID := map[string]*candidate{}

	// Seed from FTS hits
	for i, n := range ftsHits {
		byID[n.ID] = &candidate{
			node:   n,
			score:  1.0 / math.Log(float64(i+2)),
			source: "fts",
			hops:   0,
		}
	}

	// Graph expansion: single multi-seed CTE call replaces the per-seed BFS loop.
	seedIDs := make([]string, len(ftsHits))
	for i, h := range ftsHits {
		seedIDs[i] = h.ID
	}
	hopMap, _, err := b.store.TraceEdgesMulti(seedIDs, graphDepth)
	if err == nil {
		// Collect IDs not already in byID (FTS hits) for batch node fetch.
		var pendingIDs []string
		for toID := range hopMap {
			if _, seen := byID[toID]; !seen {
				pendingIDs = append(pendingIDs, toID)
			}
		}
		if len(pendingIDs) > 0 {
			fetched, fetchErr := b.store.GetNodesIn(pendingIDs)
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
	}

	// Sort by composite score
	candidates := make([]*candidate, 0, len(byID))
	for _, c := range byID {
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		si := candidates[i].score + typeBoost(candidates[i].node.Type)
		sj := candidates[j].score + typeBoost(candidates[j].node.Type)
		return si > sj
	})

	// Fill token budget with compressed (stage-1) content
	var nodes []ContextNode
	used := 0
	anySkipped := false

	for _, c := range candidates {
		compressed := compress(c.node)
		tokens := estimateTokens(compressed)
		if used+tokens > maxTokens {
			compressed = compressHard(c.node)
			tokens = estimateTokens(compressed)
			if used+tokens > maxTokens {
				anySkipped = true
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

	// Build branch map: directories adjacent to result (Corpus2Skill bird's-eye view)
	includedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		includedIDs[n.ID] = true
	}
	branches := b.computeBranches(includedIDs)

	// Derive suggested_next: top-2 uncovered branches by file count
	var suggestedNext []string
	for i, br := range branches {
		if i >= 2 {
			break
		}
		suggestedNext = append(suggestedNext, br.Path)
	}

	return Result{
		Nodes:         nodes,
		Branches:      branches,
		TokenEstimate: used,
		Truncated:     anySkipped,
		Insufficient:  anySkipped || len(branches) > 0,
		SuggestedNext: suggestedNext,
	}, nil
}

// DrillDown returns full (uncompressed) content for all nodes under path.
// Corresponds to Corpus2Skill's agent "drilling into a topic branch."
func (b *Builder) DrillDown(path string) (Result, error) {
	// Collect all nodes whose ID starts with path
	allIDs, err := b.store.GetAllFileNodeIDs()
	if err != nil {
		return Result{}, fmt.Errorf("list nodes: %w", err)
	}

	// Determine prefix for matching.
	// If path exactly matches a node ID it is a single file; otherwise treat it
	// as a directory prefix. We derive this from the actual node list so that
	// root-level directories (which contain no "/") are handled correctly.
	dirPrefix := strings.TrimSuffix(path, "/") + "/"
	isExactFile := false
	for _, id := range allIDs {
		if id == path {
			isExactFile = true
			break
		}
	}

	// Collect matching IDs, then batch-fetch node content in one query.
	var matchedIDs []string
	for _, id := range allIDs {
		if isExactFile {
			if id == path {
				matchedIDs = append(matchedIDs, id)
			}
		} else {
			if id == path || strings.HasPrefix(id, dirPrefix) {
				matchedIDs = append(matchedIDs, id)
			}
		}
	}

	fetched, err := b.store.GetNodesIn(matchedIDs)
	if err != nil {
		return Result{}, fmt.Errorf("fetch nodes: %w", err)
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

	var suggestedNext []string
	for i, br := range branches {
		if i >= 2 {
			break
		}
		suggestedNext = append(suggestedNext, br.Path)
	}

	return Result{
		Nodes:         nodes,
		Branches:      branches,
		TokenEstimate: used,
		Truncated:     anySkipped,
		Insufficient:  anySkipped || len(branches) > 0,
		SuggestedNext: suggestedNext,
	}, nil
}

func typeBoost(nodeType string) float64 {
	switch nodeType {
	case "function", "type", "variable":
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
	case "function", "type", "variable":
		if n.Content != "" {
			// Ensure we don't return too much for a single symbol
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
	case "function", "type", "variable":
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
	lines := strings.Split(content, "\n")
	var out []string

	switch nodeType {
	case "go_file":
		inImport := false
		braceDepth := 0
		for _, line := range lines {
			s := strings.TrimSpace(line)
			if strings.HasPrefix(s, "package ") || strings.HasPrefix(s, "import ") {
				out = append(out, line)
				if strings.Contains(s, "(") {
					inImport = true
				}
				continue
			}
			if inImport {
				out = append(out, line)
				if s == ")" {
					inImport = false
				}
				continue
			}
			if braceDepth == 0 && (strings.HasPrefix(s, "func ") ||
				strings.HasPrefix(s, "type ") ||
				strings.HasPrefix(s, "var ") ||
				strings.HasPrefix(s, "const ")) {
				out = append(out, line)
			}
			braceDepth += strings.Count(s, "{") - strings.Count(s, "}")
			if braceDepth < 0 {
				braceDepth = 0
			}
		}
	case "py_file":
		for _, line := range lines {
			s := strings.TrimSpace(line)
			if strings.HasPrefix(s, "import ") || strings.HasPrefix(s, "from ") ||
				strings.HasPrefix(s, "def ") || strings.HasPrefix(s, "class ") ||
				strings.HasPrefix(s, "async def ") {
				out = append(out, line)
			}
		}
	default:
		return truncateLines(content, maxLines)
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
