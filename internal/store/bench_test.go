package store_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dainbe/Sieve/internal/store"
)

func setupBenchStore(b *testing.B, nodes int, edges int) *store.Store {
	b.Helper()
	s, err := store.New(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	err = s.WithBatch(func(batch *store.Batch) error {
		for i := 0; i < nodes; i++ {
			id := fmt.Sprintf("file%04d.go", i)
			if err := batch.UpsertNode(id, "go_file", fmt.Sprintf("package main\n// file %d\nfunc Foo%d() {}", i, i), "h"); err != nil {
				return err
			}
		}
		for i := 0; i < edges && i+1 < nodes; i++ {
			from := fmt.Sprintf("file%04d.go", i)
			to := fmt.Sprintf("file%04d.go", i+1)
			if err := batch.UpsertEdge(from, to, "imports"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatalf("seed: %v", err)
	}
	return s
}

func BenchmarkFTSSearch(b *testing.B) {
	s := setupBenchStore(b, 1000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.FTSSearch("Foo", 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTraceEdgesMulti(b *testing.B) {
	s := setupBenchStore(b, 500, 1000)
	seeds := make([]string, 10)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("file%04d.go", i*50)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.TraceEdgesMulti(seeds, 2); err != nil {
			b.Fatal(err)
		}
	}
}
