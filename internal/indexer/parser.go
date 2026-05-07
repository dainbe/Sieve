package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ParserManager handles loading and execution of Wasm-based parsers.
// Each language parser is compiled once and cached; instances are pooled
// to avoid per-request instantiation cost.
type ParserManager struct {
	ctx     context.Context
	runtime wazero.Runtime
	dir     string

	mu      sync.RWMutex
	modules map[string]wazero.CompiledModule // compiled, reused
}

// NewParserManager creates a ParserManager rooted at dir.
// A background context is used for the wazero runtime lifetime.
func NewParserManager(dir string) (*ParserManager, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create parsers dir: %w", err)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	return &ParserManager{
		ctx:     ctx,
		runtime: r,
		dir:     dir,
		modules: make(map[string]wazero.CompiledModule),
	}, nil
}

// Close releases all wazero resources.
func (pm *ParserManager) Close() error {
	return pm.runtime.Close(pm.ctx)
}

// ClearCache clears the compiled module cache.
func (pm *ParserManager) ClearCache() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.modules = make(map[string]wazero.CompiledModule)
}

// Parse runs the Wasm parser for lang against code and returns a JSON string.
// Returns an error if no parser Wasm file exists for lang.
//
// Wasm ABI expected:
//   - malloc(size uint32) uint32
//   - free(ptr uint32)
//   - parse(ptr uint32, len uint32) uint32  → pointer to null-terminated JSON
func (pm *ParserManager) Parse(ctx context.Context, lang, code string) (string, error) {
	compiled, err := pm.getCompiled(lang)
	if err != nil {
		return "", err
	}

	mod, err := pm.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return "", fmt.Errorf("instantiate parser %s: %w", lang, err)
	}
	defer mod.Close(ctx)

	malloc := mod.ExportedFunction("malloc")
	free := mod.ExportedFunction("free")
	parse := mod.ExportedFunction("parse")
	if malloc == nil || free == nil || parse == nil {
		return "", fmt.Errorf("parser %s missing required exports (malloc/free/parse)", lang)
	}

	// Write code into Wasm memory
	codeBytes := []byte(code)
	res, err := malloc.Call(ctx, uint64(len(codeBytes)))
	if err != nil {
		return "", fmt.Errorf("malloc: %w", err)
	}
	codePtr := uint32(res[0])
	defer free.Call(ctx, uint64(codePtr)) //nolint:errcheck

	if !mod.Memory().Write(codePtr, codeBytes) {
		return "", fmt.Errorf("write to wasm memory failed")
	}

	// Call parse
	res, err = parse.Call(ctx, uint64(codePtr), uint64(len(codeBytes)))
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	resultPtr := uint32(res[0])
	if resultPtr == 0 {
		return "[]", nil
	}
	defer free.Call(ctx, uint64(resultPtr)) //nolint:errcheck

	// Read null-terminated JSON from Wasm memory
	mem := mod.Memory()
	var out []byte
	for i := resultPtr; ; i++ {
		b, ok := mem.ReadByte(i)
		if !ok || b == 0 {
			break
		}
		out = append(out, b)
	}
	return string(out), nil
}

// getCompiled returns a cached compiled module, loading from disk if needed.
// Returns an error if the .wasm file is not present — no automatic download.
func (pm *ParserManager) getCompiled(lang string) (wazero.CompiledModule, error) {
	pm.mu.RLock()
	mod, ok := pm.modules[lang]
	pm.mu.RUnlock()
	if ok {
		return mod, nil
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()
	// Double-check after acquiring write lock
	if mod, ok := pm.modules[lang]; ok {
		return mod, nil
	}

	wasmPath := filepath.Join(pm.dir, lang+".wasm")
	data, err := os.ReadFile(wasmPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no parser found for %q — place %s.wasm in %s", lang, lang, pm.dir)
		}
		return nil, fmt.Errorf("read parser %s: %w", lang, err)
	}

	compiled, err := pm.runtime.CompileModule(pm.ctx, data)
	if err != nil {
		return nil, fmt.Errorf("compile parser %s: %w", lang, err)
	}

	pm.modules[lang] = compiled
	return compiled, nil
}
