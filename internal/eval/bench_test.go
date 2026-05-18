//go:build eval

package eval_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dainbe/Sieve/internal/eval"
	"github.com/dainbe/Sieve/internal/indexer"
	"github.com/dainbe/Sieve/internal/store"
)

// BenchmarkIndexProject_RealRepo measures a full re-index of the Sieve repo itself.
// Run with: go test -tags eval -bench=BenchmarkIndexProject -benchmem ./internal/eval/...
func BenchmarkIndexProject_RealRepo(b *testing.B) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := store.New(filepath.Join(b.TempDir(), "bench.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		_, err = indexer.IndexProject(context.Background(), s, nil, repoRoot, repoRoot)
		b.StopTimer()
		_ = s.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuild_RealRepo measures ctx_build_context against the Sieve repo.
// Each iteration runs all 15 eval queries sequentially.
func BenchmarkBuild_RealRepo(b *testing.B) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		b.Fatal(err)
	}
	dbPath := filepath.Join(b.TempDir(), "bench.db")
	r, err := eval.NewRunner(repoRoot, dbPath)
	if err != nil {
		b.Fatalf("NewRunner: %v", err)
	}
	defer func() { _ = r.Close() }()

	casesDir := filepath.Join(repoRoot, "testdata", "eval", "cases")
	cases, err := eval.LoadCases(casesDir)
	if err != nil || len(cases) == 0 {
		b.Skip("no eval cases")
	}

	// Measure heap before bench loop for resident memory baseline.
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eval.RunQueries(r, cases)
	}
	b.StopTimer()

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	heapDeltaKB := float64(memAfter.HeapAlloc-memBefore.HeapAlloc) / 1024
	b.ReportMetric(heapDeltaKB, "heap_delta_KB")
	b.ReportMetric(float64(memAfter.HeapInuse)/1024/1024, "heap_inuse_MB")
}
