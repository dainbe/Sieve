package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestParserManager_NewClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "parsers")
	pm, err := NewParserManager(dir)
	if err != nil {
		t.Fatalf("NewParserManager: %v", err)
	}
	if err := pm.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestParserManager_ParseMissingLang(t *testing.T) {
	pm, err := NewParserManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewParserManager: %v", err)
	}
	defer func() { _ = pm.Close() }()

	_, err = pm.Parse(context.Background(), "no_such_lang", "code")
	if err == nil {
		t.Fatal("expected error for missing Wasm parser")
	}
	if !strings.Contains(err.Error(), "no parser found") {
		t.Errorf("expected 'no parser found' error, got: %v", err)
	}
}
