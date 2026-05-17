package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dainbe/Sieve/internal/store"
)

func BenchmarkIndexProject_SmallTree(b *testing.B) {
	// Build a temp tree with 50 Go files once; re-index in each iteration.
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "bench.db")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		content := fmt.Sprintf("package main\n\nimport \"fmt\"\n\nfunc Fn%d() { fmt.Println(%d) }\n", i, i)
		path := filepath.Join(projectDir, fmt.Sprintf("file%02d.go", i))
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			b.Fatal(err)
		}
	}

	s, err := store.New(dbPath)
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	// Prime the index so subsequent runs measure incremental (hash-check) path.
	if _, err := IndexProject(ctx, s, nil, "", projectDir); err != nil {
		b.Fatalf("prime IndexProject: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := IndexProject(ctx, s, nil, "", projectDir); err != nil {
			b.Fatal(err)
		}
	}
}
