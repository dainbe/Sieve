// Package sandbox executes untrusted Wasm binaries in an isolated environment
// using wazero (pure-Go Wasm runtime, no Docker/CGO required).
//
// Security constraints enforced by this sandbox:
//   - No host filesystem access (WASI preopened dirs: none)
//   - No host network access
//   - CPU time limited via context deadline (default 10 s)
//   - Memory limited to maxMemoryPages × 64 KiB (default 64 MiB)
//   - stdout/stderr captured and returned; stdin optionally provided
package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

const (
	defaultTimeout  = 10 * time.Second
	maxMemoryPages  = 1024        // 64 MiB  (1 page = 64 KiB)
	maxOutputBytes  = 64 * 1024   // truncate stdout/stderr at 64 KiB
)

// RunOptions controls sandbox behaviour.
type RunOptions struct {
	// Timeout overrides defaultTimeout (0 = use default).
	Timeout time.Duration
	// Stdin is the optional stdin payload fed to the Wasm module.
	Stdin string
	// Env passes key=value pairs as WASI environment variables.
	Env []string
}

// Run decodes a base64 Wasm binary, executes it in a sandboxed WASI
// environment, and returns the combined stdout output (truncated at 64 KiB).
func Run(ctx context.Context, wasmB64, stdin string) (string, error) {
	return RunWithOptions(ctx, wasmB64, RunOptions{Stdin: stdin})
}

// RunWithOptions is the full-featured entry point.
func RunWithOptions(ctx context.Context, wasmB64 string, opts RunOptions) (string, error) {
	// 1. Decode
	wasm, err := base64.StdEncoding.DecodeString(wasmB64)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if len(wasm) < 4 || string(wasm[:4]) != "\x00asm" {
		return "", fmt.Errorf("invalid wasm binary (bad magic bytes)")
	}

	// 2. Timeout
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 3. Runtime — created fresh per call because each invocation receives an
	//    arbitrary Wasm binary that cannot be shared across calls.
	//    The ParserManager (indexer package) uses a cached runtime for
	//    repeated calls with the same Wasm; that pattern does not apply here.
	rCfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(maxMemoryPages).
		WithCloseOnContextDone(true)
	r := wazero.NewRuntimeWithConfig(ctx, rCfg)
	defer r.Close(ctx)

	// 4. WASI
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// 5. I/O buffers
	var stdout, stderr bytes.Buffer
	stdinReader := strings.NewReader(opts.Stdin)

	// 6. Module config — no filesystem, no network
	modCfg := wazero.NewModuleConfig().
		WithStdin(stdinReader).
		WithStdout(&limitWriter{w: &stdout, limit: maxOutputBytes}).
		WithStderr(&limitWriter{w: &stderr, limit: maxOutputBytes}).
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime()

	for _, kv := range opts.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			modCfg = modCfg.WithEnv(parts[0], parts[1])
		}
	}

	// 7. Compile + instantiate — run in a dedicated goroutine so that
	//    a CPU-spinning Wasm module is killed when the context deadline fires.
	type result struct {
		err error
	}
	done := make(chan result, 1)

	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		return "", fmt.Errorf("compile wasm: %w", err)
	}

	go func() {
		_, execErr := r.InstantiateModule(ctx, compiled, modCfg)
		done <- result{err: execErr}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("sandbox timeout after %s", timeout)
	case res := <-done:
		if res.err != nil {
			if isExitCode(res.err, 0) {
				res.err = nil
			} else if isExitError(res.err) {
				code := exitCode(res.err)
				return buildOutput(stdout.String(), stderr.String()),
					fmt.Errorf("wasm exited with code %d\nstderr: %s", code, stderr.String())
			} else {
				return "", fmt.Errorf("instantiate: %w", res.err)
			}
		}
	}

	return buildOutput(stdout.String(), stderr.String()), nil
}

func buildOutput(stdout, stderr string) string {
	out := strings.TrimRight(stdout, "\n")
	if stderr != "" {
		out += "\n[stderr] " + strings.TrimRight(stderr, "\n")
	}
	return out
}

// --- Exit error helpers (wazero wraps them in sys.ExitError) ---

func isExitError(err error) bool {
	var e *sys.ExitError
	return errors.As(err, &e)
}

func isExitCode(err error, code uint32) bool {
	var e *sys.ExitError
	if errors.As(err, &e) {
		return e.ExitCode() == code
	}
	return false
}

func exitCode(err error) uint32 {
	var e *sys.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return 0
}

// --- limitWriter caps output size ---

type limitWriter struct {
	w     *bytes.Buffer
	limit int
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.w.Len()
	if remaining <= 0 {
		return len(p), nil // silently drop
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return lw.w.Write(p)
}