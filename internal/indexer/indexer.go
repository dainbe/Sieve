package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dainbe/Sieve/internal/store"
)

// IndexProject walks root, upserts changed files, extracts edges and symbols.
// allowedRoot restricts indexing to a subtree; pass "" to allow any path under root.
// Returns the number of files actually updated.
func IndexProject(ctx context.Context, s *store.Store, pm *ParserManager, allowedRoot, root string) (int, error) {
	// Security: resolve symlinks and verify root is under allowedRoot
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// Path may not exist yet; fall back to Abs
		absRoot, err = filepath.Abs(root)
		if err != nil {
			return 0, fmt.Errorf("resolve root: %w", err)
		}
	}
	if allowedRoot != "" {
		absAllowed, err := filepath.Abs(allowedRoot)
		if err != nil {
			return 0, fmt.Errorf("resolve allowed root: %w", err)
		}
		if !strings.HasPrefix(absRoot+string(filepath.Separator), absAllowed+string(filepath.Separator)) {
			return 0, fmt.Errorf("path %q is outside allowed root %q", absRoot, absAllowed)
		}
	}

	existing, _ := s.GetAllFileNodeIDs()
	seen := make(map[string]bool)
	var updated int

	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && shouldSkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isSupportedFile(path) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// Resolve symlinks per file to prevent traversal
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			slog.Warn("indexer: skip unresolvable symlink", "path", path)
			return nil
		}
		if allowedRoot != "" {
			absAllowed, _ := filepath.Abs(allowedRoot)
			if !strings.HasPrefix(realPath+string(filepath.Separator), absAllowed+string(filepath.Separator)) {
				slog.Warn("indexer: skip path outside allowed root", "path", realPath)
				return nil
			}
		}

		data, err := os.ReadFile(realPath)
		if err != nil {
			slog.Warn("indexer: skip unreadable file", "path", path, "err", err)
			return nil
		}

		relPath, _ := filepath.Rel(absRoot, path)
		hash := hashContent(data)
		seen[relPath] = true

		if s.IsHashCurrent(relPath, hash) {
			return nil
		}

		content := string(data)
		ext := filepath.Ext(path)

		if err := s.UpsertNode(relPath, extToType(ext), content, hash); err != nil {
			return fmt.Errorf("upsert node %s: %w", relPath, err)
		}

		// Import edges
		for _, imp := range extractImports(content, ext) {
			_ = s.UpsertNode(imp, "import", "", "")
			_ = s.UpsertEdge(relPath, imp, "imports")
		}

		// Symbol extraction
		switch ext {
		case ".go":
			for _, sym := range extractGoSymbols(content) {
				symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
				_ = s.UpsertNode(symID, sym.Type, sym.Content, "")
				_ = s.UpsertEdge(relPath, symID, "contains")
			}
		default:
			if pm != nil {
				lang := extToLang(ext)
				if lang != "" {
					jsonStr, err := pm.Parse(ctx, lang, content)
					if err == nil {
						var syms []Symbol
						if err := json.Unmarshal([]byte(jsonStr), &syms); err == nil {
							for _, sym := range syms {
								symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
								_ = s.UpsertNode(symID, sym.Type, sym.Content, "")
								_ = s.UpsertEdge(relPath, symID, "contains")
							}
						}
					}
				}
			}
		}

		updated++
		slog.Debug("indexer: indexed", "path", relPath)
		return nil
	})

	// Cleanup stale nodes (files deleted since last index)
	for _, id := range existing {
		if !seen[id] {
			if err := s.DeleteNode(id); err == nil {
				slog.Info("indexer: deleted stale node", "path", id)
			}
		}
	}

	return updated, err
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func shouldSkipDir(name string) bool {
	skip := map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		".idea": true, ".vscode": true, "__pycache__": true,
	}
	return skip[name]
}

func isSupportedFile(path string) bool {
	exts := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".rs": true, ".md": true, ".txt": true,
	}
	return exts[filepath.Ext(path)]
}

func extToType(ext string) string {
	switch ext {
	case ".go":
		return "go_file"
	case ".ts", ".tsx":
		return "ts_file"
	case ".js", ".jsx":
		return "js_file"
	case ".py":
		return "py_file"
	case ".rs":
		return "rs_file"
	default:
		return "text_file"
	}
}

func extToLang(ext string) string {
	switch ext {
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	default:
		return ""
	}
}

func extractImports(content, ext string) []string {
	var imports []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch ext {
		case ".go":
			if strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `"`) {
				imports = append(imports, strings.Trim(line, `"`))
			}
		case ".py":
			if strings.HasPrefix(line, "import ") {
				imports = append(imports, strings.TrimPrefix(line, "import "))
			} else if strings.HasPrefix(line, "from ") {
				if parts := strings.Fields(line); len(parts) >= 2 {
					imports = append(imports, parts[1])
				}
			}
		case ".ts", ".tsx", ".js", ".jsx":
			if strings.Contains(line, "from '") || strings.Contains(line, `from "`) {
				q := byte('\'')
				if strings.Contains(line, `from "`) {
					q = '"'
				}
				start := strings.LastIndexByte(line, q)
				if start > 0 {
					sub := line[start+1:]
					if end := strings.IndexByte(sub, q); end >= 0 {
						imports = append(imports, sub[:end])
					}
				}
			}
		}
	}
	return imports
}
