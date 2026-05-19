// Package gitstate provides lightweight git repository state queries
// that read git internals directly without shelling out to the git binary.
package gitstate

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ReadHead returns the resolved commit SHA for HEAD in the git repository
// rooted at root. Returns ("", nil) when root is not a git repository or
// when HEAD cannot be resolved (e.g. initial empty repo).
//
// Resolution order:
//  1. Read <root>/.git/HEAD
//  2. If it is a symbolic ref ("ref: refs/heads/<name>"), follow to
//     <root>/.git/refs/heads/<name>, then fall back to packed-refs.
//  3. If it is already a 40-hex SHA (detached HEAD), return it directly.
//
// worktree support (.git being a file) is not implemented; those repos
// return ("", nil) so callers treat them as "no HEAD available".
func ReadHead(root string) (string, error) {
	gitDir := filepath.Join(root, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return "", nil
	}
	if !fi.IsDir() {
		// worktree: .git is a file pointing elsewhere — not supported
		return "", nil
	}

	headPath := filepath.Join(gitDir, "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return "", nil
	}
	line := strings.TrimSpace(string(data))

	if strings.HasPrefix(line, "ref: ") {
		ref := strings.TrimPrefix(line, "ref: ")
		sha, err := resolveRef(gitDir, ref)
		if err != nil {
			return "", err
		}
		return sha, nil
	}

	// detached HEAD: line is the SHA itself
	if isHexSHA(line) {
		return line, nil
	}
	return "", nil
}

// resolveRef resolves a symbolic ref like "refs/heads/main" to a commit SHA.
// It first checks the loose ref file, then falls back to packed-refs.
func resolveRef(gitDir, ref string) (string, error) {
	loose := filepath.Join(gitDir, filepath.FromSlash(ref))
	data, err := os.ReadFile(loose)
	if err == nil {
		sha := strings.TrimSpace(string(data))
		if isHexSHA(sha) {
			return sha, nil
		}
	}

	// packed-refs fallback
	return readPackedRef(gitDir, ref)
}

// readPackedRef searches <gitDir>/packed-refs for the given ref name.
func readPackedRef(gitDir, ref string) (string, error) {
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			if isHexSHA(fields[0]) {
				return fields[0], nil
			}
		}
	}
	return "", scanner.Err()
}

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
