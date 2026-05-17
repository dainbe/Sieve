package context_test

import (
	"fmt"
	"testing"

	ctx "github.com/dainbe/Sieve/internal/context"
	"github.com/dainbe/Sieve/internal/store"
)

func BenchmarkBuilder_Build(b *testing.B) {
	s, err := store.New(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	// Seed with 200 nodes and a chain of edges.
	err = s.WithBatch(func(batch *store.Batch) error {
		for i := 0; i < 200; i++ {
			id := fmt.Sprintf("pkg/file%03d.go", i)
			content := fmt.Sprintf("package pkg\n// benchtoken %d\nfunc Fn%d() {}", i, i)
			if err := batch.UpsertNode(id, "go_file", content, "h"); err != nil {
				return err
			}
			if i > 0 {
				prev := fmt.Sprintf("pkg/file%03d.go", i-1)
				if err := batch.UpsertEdge(prev, id, "imports"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatalf("seed: %v", err)
	}

	builder := ctx.NewBuilder(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := builder.Build("benchtoken"); err != nil {
			b.Fatal(err)
		}
	}
}
