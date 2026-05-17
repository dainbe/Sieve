package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/sys"
)

func TestSandbox_InvalidBase64(t *testing.T) {
	_, err := Run(context.Background(), "!!not-base64!!", "")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("expected base64 error, got: %v", err)
	}
}

func TestSandbox_BadMagic(t *testing.T) {
	// "aGVsbG8=" = base64("hello") — valid base64 but not a Wasm binary
	_, err := Run(context.Background(), "aGVsbG8=", "")
	if err == nil {
		t.Fatal("expected error for bad Wasm magic bytes")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("expected magic bytes error, got: %v", err)
	}
}

func TestSandbox_BuildOutput(t *testing.T) {
	cases := []struct {
		stdout, stderr, want string
	}{
		{"hello\n", "", "hello"},
		{"hello\n", "", "hello"},
		{"a", "warn", "a\n[stderr] warn"},
		{"", "", ""},
		{"line\n", "err\n", "line\n[stderr] err"},
	}
	for _, tc := range cases {
		got := buildOutput(tc.stdout, tc.stderr)
		if got != tc.want {
			t.Errorf("buildOutput(%q, %q) = %q, want %q", tc.stdout, tc.stderr, got, tc.want)
		}
	}
}

func TestSandbox_LimitWriter(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitWriter{w: &buf, limit: 10}

	n, err := lw.Write([]byte("hello world!")) // 12 bytes > limit 10
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// limitWriter returns the number of bytes actually written (capped at remaining).
	if n != 10 {
		t.Errorf("expected Write to return 10 (bytes written up to limit), got %d", n)
	}
	if buf.Len() != 10 {
		t.Errorf("expected buffer len 10, got %d", buf.Len())
	}
	if buf.String() != "hello worl" {
		t.Errorf("unexpected buffer content: %q", buf.String())
	}

	// Writing more when at limit is a no-op.
	n, err = lw.Write([]byte("more"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 {
		t.Errorf("expected Write to return 4, got %d", n)
	}
	if buf.Len() != 10 {
		t.Errorf("buffer should remain at 10, got %d", buf.Len())
	}
}

func TestSandbox_ExitHelpers(t *testing.T) {
	exitErr := sys.NewExitError(42)

	if !isExitError(exitErr) {
		t.Error("isExitError should be true for sys.ExitError")
	}
	if isExitError(nil) {
		t.Error("isExitError should be false for nil")
	}

	if !isExitCode(exitErr, 42) {
		t.Error("isExitCode(exitErr, 42) should be true")
	}
	if isExitCode(exitErr, 0) {
		t.Error("isExitCode(exitErr, 0) should be false")
	}
	if isExitCode(nil, 0) {
		t.Error("isExitCode(nil, 0) should be false")
	}

	if code := exitCode(exitErr); code != 42 {
		t.Errorf("exitCode: want 42, got %d", code)
	}
	if code := exitCode(nil); code != 0 {
		t.Errorf("exitCode(nil): want 0, got %d", code)
	}
}
