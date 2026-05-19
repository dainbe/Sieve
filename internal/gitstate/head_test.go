package gitstate

import (
	"os"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadHead_NoGitDir(t *testing.T) {
	dir := t.TempDir()
	sha, err := ReadHead(dir)
	if err != nil || sha != "" {
		t.Fatalf("expected (\"\", nil), got (%q, %v)", sha, err)
	}
}

func TestReadHead_SymbolicRef(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	const want = "abc1234567890123456789012345678901234abc"
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, ".git", "refs", "heads", "main"), want+"\n")

	sha, err := ReadHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != want {
		t.Fatalf("got %q, want %q", sha, want)
	}
}

func TestReadHead_DetachedHEAD(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	const want = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), want+"\n")

	sha, err := ReadHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != want {
		t.Fatalf("got %q, want %q", sha, want)
	}
}

func TestReadHead_PackedRefs(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	const want = "cafebabecafebabecafebabecafebabecafebabe"
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/feature\n")
	writeFile(t, filepath.Join(dir, ".git", "packed-refs"),
		"# pack-refs with: peeled fully-peeled sorted\n"+
			want+" refs/heads/feature\n")

	sha, err := ReadHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != want {
		t.Fatalf("got %q, want %q", sha, want)
	}
}

func TestReadHead_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	// fresh repo: HEAD points to refs/heads/main but the ref file doesn't exist yet
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	sha, err := ReadHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "" {
		t.Fatalf("expected empty SHA for unborn branch, got %q", sha)
	}
}
