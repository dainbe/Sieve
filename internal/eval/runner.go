//go:build eval

// Package eval provides a precision/recall evaluation harness for Sieve's
// ctx_build_context quality. It is only compiled with the "eval" build tag to
// avoid adding evaluation dependencies to the production binary.
//
// Usage:
//
//	go test -tags eval -timeout 300s -v ./internal/eval/... -eval-dir ./testdata/eval
package eval

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ctxbuilder "github.com/dainbe/Sieve/internal/context"
	"github.com/dainbe/Sieve/internal/embed"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
)

const (
	TokenBudget = 4000
	TopK        = 5

	tokenBudget = TokenBudget
	topK        = TopK
)

// Runner indexes a repo snapshot and evaluates ctx_build_context against cases.
type Runner struct {
	snapshotDir        string
	s                  *store.Store
	b                  *ctxbuilder.Builder
	emb                *embed.Embedder // nil when dense retrieval is disabled
	projectTotalTokens int             // sum of raw token estimates for all source files in snapshot
}

// NewRunner creates a Runner for snapshotDir, which is the project root to index.
// Only git-tracked files are indexed to prevent corpus drift from untracked files
// skewing BM25 scores between eval runs. Falls back to full index if git is unavailable.
// The caller is responsible for closing the returned store.
func NewRunner(snapshotDir, dbPath string) (*Runner, error) {
	s, err := store.New(dbPath)
	if err != nil {
		return nil, fmt.Errorf("store.New: %w", err)
	}
	tracked, trackedErr := gitTrackedFiles(snapshotDir)
	if trackedErr != nil {
		// git unavailable — fall back to full directory index
		if err := indexProject(snapshotDir, s); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("index %q: %w", snapshotDir, err)
		}
	} else {
		if err := indexProjectTracked(snapshotDir, s, tracked); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("index tracked %q: %w", snapshotDir, err)
		}
	}
	total := countProjectTokens(snapshotDir, tracked)

	b := ctxbuilder.NewBuilder(s)

	// Attach dense retrieval when SIEVE_DENSE_RETRIEVAL=1 (default off, matching main.go).
	var emb *embed.Embedder
	if os.Getenv("SIEVE_DENSE_RETRIEVAL") == "1" {
		var embErr error
		emb, embErr = embed.New(context.Background())
		if embErr != nil {
			slog.Warn("eval: dense retrieval disabled", "err", embErr)
		} else {
			if idxErr := embedFileNodes(context.Background(), s, emb); idxErr != nil {
				slog.Warn("eval: file embedding failed", "err", idxErr)
			}
			vecs, loadErr := s.LoadAllVectors()
			if loadErr == nil && len(vecs) > 0 {
				b.SetEmbedder(emb, embed.NewVectorIndex(vecs))
				slog.Info("eval: dense index ready", "vectors", len(vecs))
			}
		}
	}

	return &Runner{
		snapshotDir:        snapshotDir,
		s:                  s,
		b:                  b,
		emb:                emb,
		projectTotalTokens: total,
	}, nil
}

// embedFileNodes embeds all file nodes in the store and persists the vectors.
// File content is truncated to 512 chars to keep embedding latency reasonable.
func embedFileNodes(ctx context.Context, s *store.Store, emb *embed.Embedder) error {
	fileNodes, err := s.GetAllFileNodes()
	if err != nil {
		return fmt.Errorf("list file nodes: %w", err)
	}
	if len(fileNodes) == 0 {
		return nil
	}

	const batchSize = 16
	texts := make([]string, 0, batchSize)
	ids := make([]string, 0, batchSize)

	flush := func() error {
		if len(texts) == 0 {
			return nil
		}
		vecs, embErr := emb.Embed(ctx, texts)
		if embErr != nil {
			return embErr
		}
		for i, id := range ids {
			if i < len(vecs) {
				if uErr := s.UpsertVector(id, vecs[i]); uErr != nil {
					return uErr
				}
			}
		}
		texts = texts[:0]
		ids = ids[:0]
		return nil
	}

	for _, n := range fileNodes {
		texts = append(texts, n.ID+"\n"+truncateStr(n.Content, 512))
		ids = append(ids, n.ID)
		if len(texts) >= batchSize {
			if fErr := flush(); fErr != nil {
				slog.Warn("eval: embed batch failed", "err", fErr)
				texts = texts[:0]
				ids = ids[:0]
			}
		}
	}
	return flush()
}

// gitTrackedFiles returns the set of relative paths tracked by git in dir.
func gitTrackedFiles(dir string) (map[string]struct{}, error) {
	out, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			set[p] = struct{}{}
		}
	}
	return set, nil
}

// indexProjectTracked copies only git-tracked files to a temp directory and
// indexes that, so BM25 scores are stable regardless of untracked files present
// in snapshotDir.
func indexProjectTracked(snapshotDir string, s *store.Store, tracked map[string]struct{}) error {
	tmpDir, err := os.MkdirTemp("", "sieve-eval-*")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	for relPath := range tracked {
		src := filepath.Join(snapshotDir, relPath)
		dst := filepath.Join(tmpDir, relPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	_, err = indexer.IndexProject(context.Background(), s, nil, tmpDir, tmpDir)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// countProjectTokens sums estimated tokens for source files in dir.
// If tracked is non-nil, only those relative paths are counted (git-tracked files).
// Otherwise all text source files are counted. Uses len(bytes)/4 as token estimate.
func countProjectTokens(dir string, tracked map[string]struct{}) int {
	if tracked != nil {
		var total int
		for relPath := range tracked {
			ext := strings.ToLower(filepath.Ext(relPath))
			switch ext {
			case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rb", ".rs", ".java",
				".c", ".cpp", ".h", ".hpp", ".cs", ".md", ".txt", ".yaml", ".yml",
				".json", ".toml", ".sql":
				data, err := os.ReadFile(filepath.Join(dir, relPath))
				if err == nil {
					total += len(data) / 4
				}
			}
		}
		return total
	}
	skip := map[string]bool{".git": true, "vendor": true, "node_modules": true}
	var total int
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skip[d.Name()] {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rb", ".rs", ".java",
			".c", ".cpp", ".h", ".hpp", ".cs", ".md", ".txt", ".yaml", ".yml",
			".json", ".toml", ".sql":
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				total += len(data) / 4
			}
		}
		return nil
	})
	return total
}

func (r *Runner) Close() error {
	if r.emb != nil {
		r.emb.Close()
	}
	return r.s.Close()
}

func truncateStr(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// Eval runs all cases and returns per-case Metrics and a Summary.
func (r *Runner) Eval(cases []Case) ([]Metrics, Summary) {
	var results []Metrics
	for _, c := range cases {
		m := r.evalOne(c)
		m.Compute(tokenBudget)
		results = append(results, m)
	}
	return results, Summarize(results)
}

func (r *Runner) evalOne(c Case) Metrics {
	t0 := time.Now()
	res, err := r.b.Build(c.Query)
	m := Metrics{
		CaseID:         c.ID,
		Query:          c.Query,
		K:              topK,
		GroundTruth:    c.GroundTruth,
		BuildLatencyMS: float64(time.Since(t0).Microseconds()) / 1000.0,
	}
	if err != nil {
		return m
	}
	m.TokenEstimate = res.TokenEstimate

	// Estimate raw token count for GT files (used for TokenRatio).
	for _, gtPath := range c.GroundTruth {
		data, readErr := os.ReadFile(filepath.Join(r.snapshotDir, gtPath))
		if readErr == nil {
			m.RawFileTokens += len(data) / 4
		}
	}
	if m.RawFileTokens > 0 {
		m.TokenRatio = float64(m.TokenEstimate) / float64(m.RawFileTokens)
	}

	// CompressionRatio: fraction of project-total tokens returned.
	if r.projectTotalTokens > 0 {
		m.CompressionRatio = float64(m.TokenEstimate) / float64(r.projectTotalTokens)
	}

	// InformationDensity: fraction of returned tokens that come from GT files.
	// Approximate per-node token count as len(content)/4.
	// Deduplicate by file ID so a GT file that appears as both a file node and
	// one or more symbol nodes is only counted once.
	gtSet := toFileSet(c.GroundTruth)
	var usefulTokens int
	seenGTFile := make(map[string]bool)
	dump := os.Getenv("SIEVE_EVAL_DUMP") == "1"
	for i, n := range res.Nodes {
		fileID := n.ID
		if idx := findLastColon(n.ID); idx >= 0 {
			fileID = n.ID[:idx]
		}
		nodeToks := len(n.Content) / 4
		if gtSet[fileID] && !seenGTFile[fileID] {
			seenGTFile[fileID] = true
			usefulTokens += nodeToks
		}
		if dump && i < 10 {
			fmt.Printf("  [%s] rank=%d id=%s score=%.4f src=%s\n", m.CaseID, i+1, fileID, n.Score, n.Source)
		}
		m.Retrieved = appendUniq(m.Retrieved, fileID)
	}
	if m.TokenEstimate > 0 {
		m.InformationDensity = math.Round(float64(usefulTokens)/float64(m.TokenEstimate)*1000) / 1000
	}
	// EfficiencyScore is computed in Compute() after PrecisionAtK is available.
	return m
}

// toFileSet converts a list of file paths into a lookup set.
func toFileSet(paths []string) map[string]bool {
	s := make(map[string]bool, len(paths))
	for _, p := range paths {
		s[p] = true
	}
	return s
}

func indexProject(dir string, s *store.Store) error {
	_, err := indexer.IndexProject(context.Background(), s, nil, dir, dir)
	return err
}

func findLastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func appendUniq(slice []string, v string) []string {
	for _, e := range slice {
		if e == v {
			return slice
		}
	}
	return append(slice, v)
}

// BaselinePaths returns the top-K file node IDs matching query via simple FTS,
// without graph expansion or compression, for comparison with Sieve's result.
func BaselinePaths(s *store.Store, query string, k int) []string {
	nodes, err := s.FTSSearch(query, k)
	if err != nil {
		return nil
	}
	var paths []string
	for _, n := range nodes {
		fileID := n.ID
		if idx := findLastColon(n.ID); idx >= 0 {
			fileID = n.ID[:idx]
		}
		paths = appendUniq(paths, fileID)
	}
	return paths
}

// SnapshotDir returns the indexed snapshot path for this runner.
func (r *Runner) SnapshotDir() string { return r.snapshotDir }

// RunQueries calls Build for each case and discards results. Used by benchmarks
// to measure throughput without eval overhead.
func RunQueries(r *Runner, cases []Case) {
	for _, c := range cases {
		_, _ = r.b.Build(c.Query)
	}
}
