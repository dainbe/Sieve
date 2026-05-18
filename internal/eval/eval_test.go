//go:build eval

package eval_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dainbe/Sieve/internal/eval"
)

var evalDir = flag.String("eval-dir", "../../testdata/eval", "directory containing eval cases and snapshots")

// TestEval_SieveRepo evaluates ctx_build_context against the Sieve repo itself.
// It requires the cases in testdata/eval/cases/ and uses the current source tree
// as the snapshot (no separate download needed).
func TestEval_SieveRepo(t *testing.T) {
	casesDir := filepath.Join(*evalDir, "cases")
	cases, err := eval.LoadCases(casesDir)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) == 0 {
		t.Skip("no eval cases found — add JSON files to testdata/eval/cases/")
	}

	// Use the repo root (two levels up from this package) as the snapshot.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "eval.db")
	r, err := eval.NewRunner(repoRoot, dbPath)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("runner close: %v", err)
		}
	})

	metrics, summary := r.Eval(cases)

	// Print per-case results
	t.Logf("\n%-40s  P@%d  R@%d   MRR  nDCG@%d  tokens  compr   dens   eff   lat_ms", "case", eval.TopK, eval.TopK, eval.TopK)
	t.Logf("%s", strings.Repeat("-", 110))
	for _, m := range metrics {
		t.Logf("%-40s  %.3f  %.3f  %.3f  %.3f   %-6d  %.4f  %.3f  %.3f  %6.1f",
			m.CaseID, m.PrecisionAtK, m.RecallAtK, m.MRR, m.NDCGAtK,
			m.TokenEstimate, m.CompressionRatio, m.InformationDensity, m.EfficiencyScore, m.BuildLatencyMS)
	}
	t.Logf("%s", strings.Repeat("=", 110))
	t.Logf("%-40s  %.3f  %.3f  %.3f  %.3f   %-6.0f  %.4f  %.3f  %.3f  %6.1f",
		fmt.Sprintf("AVERAGE (n=%d)", summary.Count),
		summary.AvgPrecision, summary.AvgRecall, summary.AvgMRR, summary.AvgNDCG,
		summary.AvgTokens, summary.AvgCompressionRatio, summary.AvgInformationDensity,
		summary.AvgEfficiencyScore, summary.AvgLatencyMS)

	// Write report to file for CI/tracking
	reportPath := filepath.Join(*evalDir, "last-report.txt")
	if err := writeReport(reportPath, metrics, summary); err != nil {
		t.Errorf("write report: %v", err)
	}

	// Soft assertions (don't fail the test, just log warnings)
	// Structural ceiling: (n_multi*0.4 + n_single*0.2)/total — raising it requires
	// semantic search or eval-case changes beyond keyword tuning.
	if summary.AvgPrecision < 0.27 {
		t.Logf("WARN: avg precision %.3f is below 0.27 — consider tuning", summary.AvgPrecision)
	}
	if summary.BudgetPassPct < 90 {
		t.Logf("WARN: budget compliance %.1f%% is below 90%%", summary.BudgetPassPct)
	}
}

func writeReport(path string, metrics []eval.Metrics, summary eval.Summary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%-40s  P@5   R@5   MRR   nDCG@5  tokens  compr   dens   eff   lat_ms\n", "case"))
	for _, m := range metrics {
		sb.WriteString(fmt.Sprintf("%-40s  %.3f  %.3f  %.3f  %.3f   %-6d  %.4f  %.3f  %.3f  %6.1f\n",
			m.CaseID, m.PrecisionAtK, m.RecallAtK, m.MRR, m.NDCGAtK,
			m.TokenEstimate, m.CompressionRatio, m.InformationDensity, m.EfficiencyScore, m.BuildLatencyMS))
	}
	sb.WriteString(fmt.Sprintf("%-40s  %.3f  %.3f  %.3f  %.3f   %-6.0f  %.4f  %.3f  %.3f  %6.1f\n",
		fmt.Sprintf("AVERAGE n=%d", summary.Count),
		summary.AvgPrecision, summary.AvgRecall, summary.AvgMRR, summary.AvgNDCG,
		summary.AvgTokens, summary.AvgCompressionRatio, summary.AvgInformationDensity,
		summary.AvgEfficiencyScore, summary.AvgLatencyMS))
	return os.WriteFile(path, []byte(sb.String()), 0644)
}
