//go:build eval

package eval_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dainbe/Sieve/internal/eval"
)

func TestDumpCases(t *testing.T) {
	if os.Getenv("SIEVE_EVAL_DUMP") != "1" {
		t.Skip("set SIEVE_EVAL_DUMP=1 to run")
	}
	repoRoot, _ := filepath.Abs("../..")
	r, err := eval.NewRunner(repoRoot, filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	cases, _ := eval.LoadCases(filepath.Join(repoRoot, "testdata/eval/cases"))
	for _, c := range cases {
		if c.ID != "wasm-parser" && c.ID != "incremental-index" {
			continue
		}
		m, _ := r.Eval([]eval.Case{c})
		fmt.Printf("%s R@5=%.3f P@5=%.3f\n  GT: %v\n  Retrieved: %v\n",
			c.ID, m[0].RecallAtK, m[0].PrecisionAtK,
			m[0].GroundTruth, m[0].Retrieved)
	}
}
