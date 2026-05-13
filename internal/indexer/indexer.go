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
	absAllowed := absRoot
	if allowedRoot != "" {
		absAllowed, _ = filepath.Abs(allowedRoot)
	}
	existing, _ := s.GetAllFileNodeIDs()
	seen := make(map[string]bool)
	var updated int

	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Error("indexer: error walking path", "path", path, "err", err)
			return err
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
		ext := strings.ToLower(filepath.Ext(path))

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
			lang := extToLang(ext)
			if lang == "" {
				break
			}
			if pm == nil {
				// Fallback: heuristic extraction (no Wasm parser configured)
				for _, sym := range extractSymbolsHeuristic(ext, content) {
					symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
					_ = s.UpsertNode(symID, sym.Type, sym.Content, "")
					_ = s.UpsertEdge(relPath, symID, "contains")
				}
				break
			}

			jsonStr, err := pm.Parse(ctx, lang, content)
			if err != nil {
				slog.Warn("indexer: parser failed; skipping symbol extraction",
					"path", relPath,
					"lang", lang,
					"err", err,
				)
				break
			}

			var syms []Symbol
			if err := json.Unmarshal([]byte(jsonStr), &syms); err != nil {
				slog.Warn("indexer: parser returned invalid symbols JSON; skipping symbol extraction",
					"path", relPath,
					"lang", lang,
					"err", err,
				)
				break
			}

			for _, sym := range syms {
				symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
				_ = s.UpsertNode(symID, sym.Type, sym.Content, "")
				_ = s.UpsertEdge(relPath, symID, "contains")
			}
			if len(syms) > 0 {
				slog.Debug("indexer: parser symbols extracted",
					"path", relPath,
					"lang", lang,
					"symbols", len(syms),
				)
			}
		}

		updated++
		slog.Debug("indexer: indexed", "path", relPath)
		return nil
	})

	// Cleanup stale nodes (only those within the current scan root)
	relScanRoot, _ := filepath.Rel(absAllowed, absRoot)
	for _, id := range existing {
		// If the node is under the directory we just scanned but wasn't seen
		isUnderScanRoot := relScanRoot == "." || strings.HasPrefix(id, relScanRoot+string(filepath.Separator))

		if isUnderScanRoot && !seen[id] {
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
	ext := strings.ToLower(filepath.Ext(path))
	supported := map[string]bool{
		".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".py": true, ".rs": true, ".md": true, ".txt": true,
	}
	return supported[ext]
}

func extToType(ext string) string {
	switch strings.ToLower(ext) {
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
				// Support "import os, sys"
				mods := strings.Split(strings.TrimPrefix(line, "import "), ",")
				for _, m := range mods {
					imports = append(imports, strings.TrimSpace(m))
				}
			} else if strings.HasPrefix(line, "from ") {
				if parts := strings.Fields(line); len(parts) >= 2 {
					// Handle "from .module import func" or "from module import ..."
					mod := parts[1]
					imports = append(imports, mod)
				}
			}
		case ".ts", ".tsx", ".js", ".jsx":
			// Handle both 'import ... from "mod"' and 'import "mod"' or 'require("mod")'
			line = strings.ReplaceAll(line, "`", "\"")
			line = strings.ReplaceAll(line, "'", "\"")

			if strings.Contains(line, "import") || strings.Contains(line, "require(") {
				parts := strings.Split(line, "\"")
				// In 'import { x } from "mod"', the path is usually in the second to last part
				// In 'require("mod")', it's in the second part.
				for i := 1; i < len(parts); i += 2 {
					m := strings.TrimSpace(parts[i])
					if m != "" && m != "from" && !strings.Contains(m, " ") {
						imports = append(imports, m)
					}
				}
			}
		}
	}
	return imports
}