package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dainbe/Sieve/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestIndexProjectHandlerDoesNotDeadlock(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	projectDir := filepath.Join(tmpDir, "project")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "app.py"), []byte("import os\n\ndef main():\n    return os.getcwd()\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, "", "test")
	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]interface{}{"path": projectDir}

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		res, err := h.IndexProject(context.Background(), req)
		if err != nil {
			done <- result{err: err}
			return
		}
		if len(res.Content) == 0 {
			done <- result{err: err}
			return
		}
		text, _ := res.Content[0].(mcp.TextContent)
		done <- result{text: text.Text}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !strings.Contains(got.text, "Indexed 1 files") {
			t.Fatalf("unexpected response: %q", got.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IndexProject handler timed out, likely due to recursive store mutex locking")
	}
}

func TestIndexProjectDefaultsToAllowedRoot(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	projectDir := filepath.Join(tmpDir, "project")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "app.py"), []byte("print('ok')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, projectDir, "test")
	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]interface{}{}

	res, err := h.IndexProject(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) == 0 {
		t.Fatal("expected tool result content")
	}
	text, _ := res.Content[0].(mcp.TextContent)
	if !strings.Contains(text.Text, "Indexed 1 files") {
		t.Fatalf("unexpected response: %q", text.Text)
	}
}
