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
	"github.com/mark3labs/mcp-go/server"
)

func newHandlerForTest(t *testing.T) *Handler {
	t.Helper()
	tmpDir := t.TempDir()
	s, err := store.New(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewHandler(s, nil, tmpDir, "", "test")
}

func makeReq(args map[string]interface{}) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func isError(res *mcp.CallToolResult) bool {
	if res == nil {
		return false
	}
	return res.IsError
}

func TestHandler_BuildContext_QueryTooLong(t *testing.T) {
	h := newHandlerForTest(t)
	longQuery := strings.Repeat("x", maxQueryLen+1)
	res, err := h.BuildContext(context.Background(), makeReq(map[string]interface{}{"query": longQuery}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error result for oversized query")
	}
}

func TestHandler_BuildContext_QueryOK(t *testing.T) {
	h := newHandlerForTest(t)
	okQuery := strings.Repeat("x", maxQueryLen)
	res, err := h.BuildContext(context.Background(), makeReq(map[string]interface{}{"query": okQuery}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should not fail with validation error (may return empty context message)
	_ = res
}

func TestHandler_HybridSearch_QueryTooLong(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.HybridSearch(context.Background(), makeReq(map[string]interface{}{
		"query": strings.Repeat("y", maxQueryLen+1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error for oversized query")
	}
}

func TestHandler_HybridSearch_NegativeLimit(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.HybridSearch(context.Background(), makeReq(map[string]interface{}{
		"query": "anything",
		"limit": float64(-1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Negative limit should be corrected to default (not error)
	if isError(res) {
		t.Error("negative limit should be silently corrected, not return error")
	}
}

func TestHandler_TraceRelation_SymbolTooLong(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.TraceRelation(context.Background(), makeReq(map[string]interface{}{
		"symbol": strings.Repeat("z", maxQueryLen+1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error for oversized symbol")
	}
}

func TestHandler_TraceRelation_NegativeDepth(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.TraceRelation(context.Background(), makeReq(map[string]interface{}{
		"symbol": "any.go",
		"depth":  float64(-1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Negative depth corrected to default (2) — should not error
	if isError(res) {
		t.Error("negative depth should be corrected, not return error")
	}
}

func TestHandler_QuickExec_StdinTooLarge(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.QuickExec(context.Background(), makeReq(map[string]interface{}{
		"wasm_b64": "dGVzdA==", // short placeholder base64
		"stdin":    strings.Repeat("s", maxStdinSize+1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error for oversized stdin")
	}
}

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

	h := NewHandler(s, nil, "", "", "test")
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

func TestHandler_DrillDown_PathRequired(t *testing.T) {
	h := newHandlerForTest(t)

	// empty path
	res, err := h.DrillDown(context.Background(), makeReq(map[string]interface{}{"path": ""}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error for empty path")
	}

	// path too long
	res, err = h.DrillDown(context.Background(), makeReq(map[string]interface{}{
		"path": strings.Repeat("p", 4097),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isError(res) {
		t.Error("expected tool error for oversized path")
	}
}

func TestHandler_DrillDown_OK(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, projectDir, "", "test")
	var indexReq mcp.CallToolRequest
	indexReq.Params.Arguments = map[string]interface{}{}
	if _, err := h.IndexProject(context.Background(), indexReq); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}

	res, err := h.DrillDown(context.Background(), makeReq(map[string]interface{}{"path": "main.go"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isError(res) {
		t.Errorf("unexpected tool error: %+v", res)
	}
	if len(res.Content) == 0 {
		t.Fatal("expected content in DrillDown response")
	}
	text, _ := res.Content[0].(mcp.TextContent)
	if !strings.Contains(text.Text, "nodes") {
		t.Errorf("expected 'nodes' in DrillDown JSON response, got: %s", text.Text)
	}
}

func TestHandler_Status_ReturnsJSON(t *testing.T) {
	h := newHandlerForTest(t)
	res, err := h.Status(context.Background(), makeReq(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isError(res) {
		t.Errorf("unexpected tool error from Status")
	}
	if len(res.Content) == 0 {
		t.Fatal("expected content in Status response")
	}
	text, _ := res.Content[0].(mcp.TextContent)
	for _, key := range []string{"version", "node_count", "go_version", "uptime"} {
		if !strings.Contains(text.Text, key) {
			t.Errorf("Status JSON missing key %q: %s", key, text.Text)
		}
	}
}

func TestHandler_ResetIndex_ClearsThenReindexes(t *testing.T) {
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
	goFile := filepath.Join(projectDir, "app.go")
	if err := os.WriteFile(goFile, []byte("package main\nfunc App() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, projectDir, "", "test")
	var indexReq mcp.CallToolRequest
	indexReq.Params.Arguments = map[string]interface{}{}
	if _, err := h.IndexProject(context.Background(), indexReq); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	nodes, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if nodes == 0 {
		t.Fatal("expected nodes after IndexProject")
	}

	// Remove file and ResetIndex — store should end up empty.
	if err := os.Remove(goFile); err != nil {
		t.Fatal(err)
	}
	var resetReq mcp.CallToolRequest
	resetReq.Params.Arguments = map[string]interface{}{"confirm": "yes-delete-all"}
	res, err := h.ResetIndex(context.Background(), resetReq)
	if err != nil {
		t.Fatalf("ResetIndex: %v", err)
	}
	if isError(res) {
		t.Errorf("unexpected tool error from ResetIndex: %+v", res)
	}

	nodes, edges, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if nodes != 0 || edges != 0 {
		t.Errorf("expected empty store after ResetIndex on empty dir, got nodes=%d edges=%d", nodes, edges)
	}
}

func TestResetIndexConfirmGuard(t *testing.T) {
	h := newHandlerForTest(t)

	var req mcp.CallToolRequest
	req.Params.Arguments = map[string]interface{}{}
	res, err := h.ResetIndex(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected IsError=true when confirm is missing")
	}
	if len(res.Content) == 0 {
		t.Fatal("expected non-empty content")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent")
	}
	if !strings.Contains(tc.Text, "yes-delete-all") {
		t.Errorf("expected error mentioning yes-delete-all, got: %q", tc.Text)
	}
}

func TestHandler_RegisterTools_NoPanic(t *testing.T) {
	h := newHandlerForTest(t)
	s := server.NewMCPServer("test", "0.0.0")
	RegisterTools(s, h) // must not panic
}

// TestHandler_Init_EmptyIndex verifies Init indexes and optimizes when the store is empty.
func TestHandler_Init_EmptyIndex(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Write a Go file to index.
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, tmpDir, "", "test")
	res, err := h.Init(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := res.Content[0].(mcp.TextContent)

	if !strings.Contains(text.Text, `"ready": true`) {
		t.Errorf("expected ready=true, got: %s", text.Text)
	}
	if !strings.Contains(text.Text, `"newly_indexed": 1`) {
		t.Errorf("expected newly_indexed=1, got: %s", text.Text)
	}
	if !strings.Contains(text.Text, `"optimized": true`) {
		t.Errorf("expected optimized=true, got: %s", text.Text)
	}
}

// TestHandler_Init_AlreadyIndexed verifies Init optimizes without re-indexing when data exists.
func TestHandler_Init_AlreadyIndexed(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := os.WriteFile(filepath.Join(tmpDir, "util.go"), []byte("package main\nfunc Helper() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(s, nil, tmpDir, "", "test")

	// First init builds the index.
	if _, err := h.Init(context.Background(), mcp.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}

	// Second init should not re-index.
	res, err := h.Init(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := res.Content[0].(mcp.TextContent)

	if !strings.Contains(text.Text, `"ready": true`) {
		t.Errorf("expected ready=true, got: %s", text.Text)
	}
	if !strings.Contains(text.Text, `"newly_indexed": 0`) {
		t.Errorf("expected newly_indexed=0 on second call, got: %s", text.Text)
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

	h := NewHandler(s, nil, projectDir, "", "test")
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
