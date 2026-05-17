package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
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
	absRoot, absAllowed, err := resolveRoots(allowedRoot, root)
	if err != nil {
		return 0, err
	}

	existing, err := s.GetAllFileNodeIDs()
	if err != nil {
		slog.Warn("indexer: failed to load existing node IDs; stale cleanup skipped", "err", err)
	}

	seen := make(map[string]bool)
	var updated int
	relScanRoot, _ := filepath.Rel(absAllowed, absRoot)

	batchErr := s.WithBatch(func(b *store.Batch) error {
		walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
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
			didUpdate, err := processFile(ctx, b, pm, absAllowed, allowedRoot, path, d, seen)
			if err != nil {
				return err
			}
			if didUpdate {
				updated++
			}
			return nil
		})
		cleanupStale(b, existing, seen, relScanRoot)
		return walkErr
	})

	return updated, batchErr
}

// resolveRoots validates allowedRoot/root and returns their resolved absolute paths.
func resolveRoots(allowedRoot, root string) (absRoot, absAllowed string, err error) {
	absRoot, err = filepath.EvalSymlinks(root)
	if err != nil {
		absRoot, err = filepath.Abs(root)
		if err != nil {
			return "", "", fmt.Errorf("resolve root: %w", err)
		}
	}
	absAllowed = absRoot
	if allowedRoot != "" {
		resolved, err2 := filepath.EvalSymlinks(allowedRoot)
		if err2 != nil {
			resolved, err2 = filepath.Abs(allowedRoot)
			if err2 != nil {
				return "", "", fmt.Errorf("resolve allowed root: %w", err2)
			}
		}
		absAllowed = resolved
		if !strings.HasPrefix(absRoot+string(filepath.Separator), absAllowed+string(filepath.Separator)) {
			return "", "", fmt.Errorf("path %q is outside allowed root %q", absRoot, absAllowed)
		}
	}
	return absRoot, absAllowed, nil
}

// processFile indexes a single file into the batch. Returns true if the file was updated.
// seen is updated with the file's relPath to track which files were encountered.
func processFile(ctx context.Context, b *store.Batch, pm *ParserManager, absAllowed, allowedRoot, path string, d fs.DirEntry, seen map[string]bool) (bool, error) {
	// Skip files larger than maxFileBytes to prevent OOM on large dumps/logs.
	if info, err := d.Info(); err == nil && info.Size() > maxFileBytes {
		slog.Warn("indexer: skip oversized file", "path", path, "size", info.Size())
		return false, nil
	}

	// Only resolve symlinks for actual symlinks; regular files use path directly.
	realPath := path
	if d.Type()&fs.ModeSymlink != 0 {
		var err error
		realPath, err = filepath.EvalSymlinks(path)
		if err != nil {
			slog.Warn("indexer: skip unresolvable symlink", "path", path)
			return false, nil
		}
		if allowedRoot != "" {
			if !strings.HasPrefix(realPath+string(filepath.Separator), absAllowed+string(filepath.Separator)) {
				slog.Warn("indexer: skip path outside allowed root", "path", realPath)
				return false, nil
			}
		}
	}

	data, err := os.ReadFile(realPath)
	if err != nil {
		slog.Warn("indexer: skip unreadable file", "path", path, "err", err)
		return false, nil
	}

	relPath, _ := filepath.Rel(absAllowed, path)
	hash := hashContent(data)
	seen[relPath] = true

	if b.IsHashCurrent(relPath, hash) {
		return false, nil
	}

	// Remove stale derived content before re-indexing so renamed/deleted symbols don't persist.
	if err := b.ClearFileContents(relPath); err != nil {
		slog.Warn("indexer: clear file contents failed", "path", relPath, "err", err)
	}

	content := string(data)
	ext := strings.ToLower(filepath.Ext(path))

	if err := b.UpsertNode(relPath, extToType(ext), content, hash); err != nil {
		slog.Warn("indexer: upsert node failed, skipping file", "path", relPath, "err", err)
		return false, nil
	}

	for _, imp := range extractImports(content, ext) {
		if err := b.UpsertNode(imp, "import", "", ""); err != nil {
			slog.Warn("indexer: upsert import node failed", "import", imp, "err", err)
		}
		if err := b.UpsertEdge(relPath, imp, "imports"); err != nil {
			slog.Warn("indexer: upsert import edge failed", "from", relPath, "to", imp, "err", err)
		}
	}

	extractAndStoreSymbols(ctx, b, pm, relPath, ext, content)

	slog.Debug("indexer: indexed", "path", relPath)
	return true, nil
}

// extractAndStoreSymbols dispatches symbol extraction by extension and stores results.
func extractAndStoreSymbols(ctx context.Context, b *store.Batch, pm *ParserManager, relPath, ext, content string) {
	switch ext {
	case ".go":
		storeSymbols(b, relPath, extractGoSymbols(content))
	default:
		lang := extToLang(ext)
		if lang == "" {
			return
		}
		if pm == nil {
			storeSymbols(b, relPath, extractSymbolsHeuristic(ext, content))
			return
		}
		jsonStr, err := pm.Parse(ctx, lang, content)
		if err != nil {
			slog.Warn("indexer: parser failed; skipping symbol extraction",
				"path", relPath, "lang", lang, "err", err)
			return
		}
		var syms []Symbol
		if err := json.Unmarshal([]byte(jsonStr), &syms); err != nil {
			slog.Warn("indexer: parser returned invalid symbols JSON; skipping symbol extraction",
				"path", relPath, "lang", lang, "err", err)
			return
		}
		storeSymbols(b, relPath, syms)
		if len(syms) > 0 {
			slog.Debug("indexer: parser symbols extracted",
				"path", relPath, "lang", lang, "symbols", len(syms))
		}
	}
}

// storeSymbols upserts symbol nodes and contains-edges for a file.
func storeSymbols(b *store.Batch, relPath string, syms []Symbol) {
	for _, sym := range syms {
		symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
		if err := b.UpsertNode(symID, sym.Type, sym.Content, ""); err != nil {
			slog.Warn("indexer: upsert symbol node failed", "id", symID, "err", err)
		}
		if err := b.UpsertEdge(relPath, symID, "contains"); err != nil {
			slog.Warn("indexer: upsert symbol edge failed", "from", relPath, "to", symID, "err", err)
		}
	}
}

// cleanupStale removes file nodes that were not seen in the current scan.
func cleanupStale(b *store.Batch, existing []string, seen map[string]bool, relScanRoot string) {
	for _, id := range existing {
		isUnderScanRoot := relScanRoot == "." || strings.HasPrefix(id, relScanRoot+string(filepath.Separator))
		if isUnderScanRoot && !seen[id] {
			if err := b.DeleteNode(id); err == nil {
				slog.Info("indexer: deleted stale node", "path", id)
			}
		}
	}
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

const maxFileBytes = 2 * 1024 * 1024 // 2 MiB — skip files larger than this

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".idea": true, ".vscode": true, "__pycache__": true,
}

var supportedExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".md": true, ".txt": true,
}

func shouldSkipDir(name string) bool {
	return skipDirs[name]
}

func isSupportedFile(path string) bool {
	return supportedExts[strings.ToLower(filepath.Ext(path))]
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
	switch ext {
	case ".go":
		// Use the Go AST parser for accurate import extraction.
		// This handles block imports, single-line imports, and aliased imports.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", content, parser.ImportsOnly)
		if err != nil {
			// Fall back to the line-based heuristic if parsing fails (e.g. partial file).
			return extractGoImportsFallback(content)
		}
		for _, imp := range f.Imports {
			if imp.Path != nil {
				path := strings.Trim(imp.Path.Value, `"`)
				if path != "" {
					imports = append(imports, path)
				}
			}
		}
	case ".py":
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "import ") {
				// Support "import os, sys"
				mods := strings.Split(strings.TrimPrefix(line, "import "), ",")
				for _, m := range mods {
					imports = append(imports, strings.TrimSpace(m))
				}
			} else if strings.HasPrefix(line, "from ") {
				if parts := strings.Fields(line); len(parts) >= 2 {
					imports = append(imports, parts[1])
				}
			}
		}
	case ".ts", ".tsx", ".js", ".jsx":
		imports = append(imports, extractTSJSImports(content)...)
	}
	return imports
}

// extractGoImportsFallback is used when the Go AST parser cannot parse the file.
func extractGoImportsFallback(content string) []string {
	var imports []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `"`) {
			imports = append(imports, strings.Trim(line, `"`))
		}
	}
	return imports
}

// extractTSJSImports extracts ES module import specifiers and CommonJS require paths
// from TypeScript/JavaScript source. It strips line comments, block comments, and
// template literal interiors to reduce false positives from the old heuristic.
func extractTSJSImports(content string) []string {
	// 1. Remove block comments /* ... */ using a simple state machine.
	var buf strings.Builder
	inBlock := false
	for i := 0; i < len(content); i++ {
		if !inBlock && i+1 < len(content) && content[i] == '/' && content[i+1] == '*' {
			inBlock = true
			i++
			buf.WriteByte(' ')
			continue
		}
		if inBlock && i+1 < len(content) && content[i] == '*' && content[i+1] == '/' {
			inBlock = false
			i++
			buf.WriteByte(' ')
			continue
		}
		if !inBlock {
			buf.WriteByte(content[i])
		} else {
			// Preserve newlines inside block comments so line numbers stay consistent.
			if content[i] == '\n' {
				buf.WriteByte('\n')
			}
		}
	}
	cleaned := buf.String()

	var imports []string
	for _, rawLine := range strings.Split(cleaned, "\n") {
		// 2. Strip single-line comment (// ...) but not URL schemes (://).
		line := rawLine
		for idx := strings.Index(line, "//"); idx >= 0; idx = strings.Index(line, "//") {
			if idx >= 1 && line[idx-1] == ':' {
				// looks like http:// — find the next // after this one
				rest := line[idx+2:]
				next := strings.Index(rest, "//")
				if next < 0 {
					break
				}
				idx = idx + 2 + next
			}
			line = line[:idx]
			break
		}

		// 3. Blank out template literal contents (` ... `) to avoid false matches.
		if strings.ContainsRune(line, '`') {
			var tl strings.Builder
			inTmpl := false
			for _, ch := range line {
				if ch == '`' {
					inTmpl = !inTmpl
					tl.WriteRune(ch)
					continue
				}
				if inTmpl {
					tl.WriteRune('_')
				} else {
					tl.WriteRune(ch)
				}
			}
			line = tl.String()
		}

		// Normalise single quotes to double quotes for uniform extraction below.
		line = strings.ReplaceAll(line, "'", "\"")

		trimmed := strings.TrimSpace(line)

		// 4. Only process lines that contain an import/require statement.
		// Also match lines with `from "X"` to capture the specifier from multi-line imports
		// like `} from "module"` that appear without an `import` keyword on the same line.
		isImportLine := strings.HasPrefix(trimmed, "import ") ||
			strings.HasPrefix(trimmed, "import{") ||
			strings.HasPrefix(trimmed, "export ") || // re-exports: export { x } from "m"
			strings.Contains(trimmed, "require(") ||
			strings.Contains(trimmed, "from \"")

		if !isImportLine {
			continue
		}

		// 5. Extract double-quoted strings that look like module specifiers.
		parts := strings.Split(line, "\"")
		for i := 1; i < len(parts); i += 2 {
			m := strings.TrimSpace(parts[i])
			if m != "" && m != "from" && !strings.Contains(m, " ") {
				imports = append(imports, m)
			}
		}
	}
	return imports
}
