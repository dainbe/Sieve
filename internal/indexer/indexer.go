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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dainbe/Sieve/internal/env"
	"github.com/dainbe/Sieve/internal/expand"
	"github.com/dainbe/Sieve/internal/gitstate"
	"github.com/dainbe/Sieve/internal/store"
)

// Embedder is the interface that dense-retrieval backends must implement.
// Satisfied by embed.HugotEmbedder when dense retrieval is enabled.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// parsedFile holds the result of reading and parsing a single source file.
// Produced by worker goroutines; consumed by the serial batch writer.
// An empty relPath means the file was unreadable or otherwise skipped entirely
// (it should not be marked as "seen" and should be removed from the index).
// A non-empty relPath with empty hash means the file passed the size/symlink
// checks but could not be read; treat the same as empty relPath.
// A non-empty hash that matches the known hash means the file is unchanged;
// symbols will be nil and the batch writer should skip re-indexing.
type parsedFile struct {
	relPath  string
	hash     string
	nodeType string
	content  string // augmented content for FTS
	imports  []string
	symbols  []Symbol
}

// indexWorkers is the number of goroutines used for parallel file parsing.
// Override with SIEVE_INDEX_WORKERS; defaults to runtime.NumCPU().
var indexWorkers = func() int {
	if n := env.IntPos("SIEVE_INDEX_WORKERS", 0); n > 0 {
		return n
	}
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 1
}()

// Indexer wraps an ongoing or completed indexing run with atomic progress counters
// readable by the status handler without locking.
type Indexer struct {
	st          *store.Store
	pm          *ParserManager
	allowedRoot string
	embedder    Embedder // nil when dense retrieval is disabled

	// atomic progress counters
	indexingActive  int32
	indexingTotal   int32
	indexingDone    int32
	embeddingActive int32
	embeddedFiles   int32

	lastIndexErrMu sync.Mutex
	lastIndexError string
}

// NewIndexer creates an Indexer. embedder may be nil.
func NewIndexer(s *store.Store, pm *ParserManager, allowedRoot string, emb Embedder) *Indexer {
	return &Indexer{st: s, pm: pm, allowedRoot: allowedRoot, embedder: emb}
}

// IndexingActive returns true while IndexAll is running.
func (ix *Indexer) IndexingActive() bool { return atomic.LoadInt32(&ix.indexingActive) == 1 }

// IndexingProgress returns (done, total) file counts.
func (ix *Indexer) IndexingProgress() (int32, int32) {
	return atomic.LoadInt32(&ix.indexingDone), atomic.LoadInt32(&ix.indexingTotal)
}

// EmbeddingActive returns true while BulkEmbed is running.
func (ix *Indexer) EmbeddingActive() bool { return atomic.LoadInt32(&ix.embeddingActive) == 1 }

// EmbeddedFiles returns the number of files embedded so far.
func (ix *Indexer) EmbeddedFiles() int32 { return atomic.LoadInt32(&ix.embeddedFiles) }

// LastIndexError returns the last error from a background index run, or "".
func (ix *Indexer) LastIndexError() string {
	ix.lastIndexErrMu.Lock()
	defer ix.lastIndexErrMu.Unlock()
	return ix.lastIndexError
}

// IndexAll indexes allowedRoot in the background. Callers should run this in a goroutine.
// A second concurrent call is a no-op (guarded by CAS on indexingActive).
func (ix *Indexer) IndexAll(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&ix.indexingActive, 0, 1) {
		return
	}
	atomic.StoreInt32(&ix.indexingDone, 0)
	atomic.StoreInt32(&ix.indexingTotal, 0)
	defer atomic.StoreInt32(&ix.indexingActive, 0)

	ix.lastIndexErrMu.Lock()
	ix.lastIndexError = ""
	ix.lastIndexErrMu.Unlock()

	slog.Info("auto-index: starting", "root", ix.allowedRoot)
	n, err := IndexProject(ctx, ix.st, ix.pm, ix.allowedRoot, ix.allowedRoot)
	if err != nil {
		if ctx.Err() != nil {
			slog.Info("auto-index: cancelled")
			return
		}
		slog.Warn("auto-index: failed", "err", err)
		ix.lastIndexErrMu.Lock()
		ix.lastIndexError = err.Error()
		ix.lastIndexErrMu.Unlock()
		return
	}
	atomic.AddInt32(&ix.indexingDone, int32(n))
	slog.Info("auto-index: complete", "root", ix.allowedRoot, "updated", n)

	// Record the git HEAD at the time of successful indexing so that
	// maybeReindexOnHeadChange can detect branch switches.
	if head, hErr := gitstate.ReadHead(ix.allowedRoot); hErr == nil && head != "" {
		if sErr := ix.st.SetMeta("last_indexed_head", head); sErr != nil {
			slog.Warn("auto-index: failed to persist last_indexed_head", "err", sErr)
		}
	}
}

// BulkEmbed embeds all file nodes that do not yet have a vector stored.
// A second concurrent call is a no-op. Returns immediately if embedder is nil.
func (ix *Indexer) BulkEmbed(ctx context.Context) error {
	if ix.embedder == nil {
		return nil
	}
	if !atomic.CompareAndSwapInt32(&ix.embeddingActive, 0, 1) {
		return nil
	}
	defer atomic.StoreInt32(&ix.embeddingActive, 0)

	atomic.StoreInt32(&ix.embeddedFiles, 0)
	slog.Info("bulk-embed: starting")
	ids, err := ix.st.GetAllFileNodeIDs()
	if err != nil {
		return fmt.Errorf("bulk-embed: list files: %w", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := ix.st.GetNode(id)
		if err != nil || n.Content == "" {
			continue
		}
		vec, err := ix.embedder.Embed(ctx, n.Content)
		if err != nil {
			slog.Warn("bulk-embed: embed failed", "id", id, "err", err)
			continue
		}
		if err := ix.st.UpsertVector(id, vec); err != nil {
			slog.Warn("bulk-embed: store failed", "id", id, "err", err)
			continue
		}
		atomic.AddInt32(&ix.embeddedFiles, 1)
	}
	slog.Info("bulk-embed: done", "embedded", atomic.LoadInt32(&ix.embeddedFiles))
	return nil
}

// IndexProject walks root, upserts changed files, extracts edges and symbols.
// allowedRoot restricts indexing to a subtree; pass "" to allow any path under root.
// Returns the number of files actually updated.
//
// Parallelism: file reads and symbol parsing run across indexWorkers goroutines
// (SIEVE_INDEX_WORKERS, default runtime.NumCPU()). All DB writes remain serial
// inside a single transaction so SQLite's single-writer constraint is respected.
// Cross-file call resolution and PPMI rebuild happen after the transaction, also serial.
func IndexProject(ctx context.Context, s *store.Store, pm *ParserManager, allowedRoot, root string) (int, error) {
	absRoot, absAllowed, err := resolveRoots(allowedRoot, root)
	if err != nil {
		return 0, err
	}

	existing, err := s.GetAllFileNodeIDs()
	if err != nil {
		slog.Warn("indexer: failed to load existing node IDs; stale cleanup skipped", "err", err)
	}

	// Pre-load file hashes so workers can skip unchanged files before parsing.
	knownHashes, err := s.GetAllFileHashes()
	if err != nil {
		slog.Warn("indexer: failed to pre-load hashes; all files will be re-parsed", "err", err)
		knownHashes = map[string]string{}
	}

	relScanRoot, _ := filepath.Rel(absAllowed, absRoot)
	ignorePatterns := loadSieveIgnore(allowedRoot)

	// Phase 1: walk directory tree serially to collect file entries (fast I/O metadata only).
	type walkEntry struct {
		path string
		d    fs.DirEntry
	}
	var walkEntries []walkEntry
	if walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Error("indexer: error walking path", "path", path, "err", err)
			return err
		}
		if d.IsDir() {
			relPath, _ := filepath.Rel(absRoot, path)
			if shouldSkipDir(d.Name()) || matchesIgnore(d.Name(), relPath, ignorePatterns) {
				return filepath.SkipDir
			}
		}
		if d.IsDir() || !isSupportedFile(path) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		walkEntries = append(walkEntries, walkEntry{path, d})
		return nil
	}); walkErr != nil {
		return 0, walkErr
	}

	// Phase 2: parse files in parallel (read + hash + symbol extraction).
	nWorkers := indexWorkers
	if nWorkers > len(walkEntries) {
		nWorkers = len(walkEntries)
	}
	if nWorkers < 1 {
		nWorkers = 1
	}

	in := make(chan walkEntry, nWorkers)
	out := make(chan parsedFile, nWorkers)

	var wg sync.WaitGroup
	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for we := range in {
				out <- readAndParseFile(ctx, pm, absAllowed, allowedRoot, we.path, we.d, knownHashes)
			}
		}()
	}
	go func() {
		for _, we := range walkEntries {
			if ctx.Err() != nil {
				break
			}
			in <- we
		}
		close(in)
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	parsedFiles := make([]parsedFile, 0, len(walkEntries))
	for pf := range out {
		parsedFiles = append(parsedFiles, pf)
	}

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}

	// Phase 3: serial batch write.
	// All DB mutations happen inside a single transaction; nameIndex and pending
	// are populated here and consumed by resolveCrossFileCalls at the end.
	seen := make(map[string]bool, len(parsedFiles))
	var updated int
	var pending []pendingCall
	nameIndex := make(map[string][]symbolEntry)

	batchErr := s.WithBatch(func(b *store.Batch) error {
		for _, pf := range parsedFiles {
			if pf.relPath == "" {
				continue // unreadable or skipped entirely; do not mark as seen
			}
			seen[pf.relPath] = true
			// Authoritative hash check inside the transaction.
			if b.IsHashCurrent(pf.relPath, pf.hash) {
				continue
			}
			if err := writeParsedFile(b, pf, &pending, nameIndex); err != nil {
				slog.Warn("indexer: write file failed, skipping", "path", pf.relPath, "err", err)
				continue
			}
			updated++
		}
		cleanupStale(b, existing, seen, relScanRoot)
		resolveCrossFileCalls(b, pending, nameIndex)
		return nil
	})

	if batchErr != nil {
		return updated, batchErr
	}

	if err := buildAndStoreTermNeighbors(s, updated); err != nil {
		slog.Warn("indexer: term-neighbor build failed; query expansion unavailable", "err", err)
	}
	return updated, nil
}

// buildAndStoreTermNeighbors computes PPMI co-occurrence and atomically replaces
// the term_neighbors table. changedFiles is used to skip the rebuild when the
// change count is below SIEVE_PPMI_REBUILD_THRESHOLD.
func buildAndStoreTermNeighbors(s *store.Store, changedFiles int) error {
	tokenize := func(text string) []string {
		a := store.TokenizeFTS(text)
		b := SplitIdentifiers(text)
		seen := make(map[string]bool, len(a)+len(b))
		out := make([]string, 0, len(a)+len(b))
		for _, t := range append(a, b...) {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		return out
	}
	return expand.BuildPPMI(s, tokenize, 10, changedFiles)
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

// readAndParseFile reads a single file and extracts its content and symbols.
// Called from worker goroutines — no DB access allowed here.
// knownHashes is the pre-loaded snapshot; if the file's hash matches, symbols are
// left nil so the batch writer can skip re-indexing cheaply.
// An empty relPath in the return value signals the file should be ignored entirely
// (oversized, unresolvable symlink, or unreadable).
func readAndParseFile(ctx context.Context, pm *ParserManager, absAllowed, allowedRoot, path string, d fs.DirEntry, knownHashes map[string]string) parsedFile {
	if info, err := d.Info(); err == nil && info.Size() > maxFileBytes {
		slog.Warn("indexer: skip oversized file", "path", path, "size", info.Size())
		return parsedFile{}
	}

	realPath := path
	if d.Type()&fs.ModeSymlink != 0 {
		var err error
		realPath, err = filepath.EvalSymlinks(path)
		if err != nil {
			slog.Warn("indexer: skip unresolvable symlink", "path", path)
			return parsedFile{}
		}
		if allowedRoot != "" {
			if !strings.HasPrefix(realPath+string(filepath.Separator), absAllowed+string(filepath.Separator)) {
				slog.Warn("indexer: skip path outside allowed root", "path", realPath)
				return parsedFile{}
			}
		}
	}

	data, err := os.ReadFile(realPath)
	if err != nil {
		slog.Warn("indexer: skip unreadable file", "path", path, "err", err)
		return parsedFile{}
	}

	relPath, _ := filepath.Rel(absAllowed, path)
	hash := hashContent(data)

	// If the hash matches the pre-loaded snapshot, skip expensive parsing.
	// The batch writer will confirm with b.IsHashCurrent inside the transaction.
	if knownHashes[relPath] == hash {
		return parsedFile{relPath: relPath, hash: hash}
	}

	content := string(data)
	ext := strings.ToLower(filepath.Ext(path))

	return parsedFile{
		relPath:  relPath,
		hash:     hash,
		nodeType: extToType(ext),
		content:  augmentContent(content, relPath),
		imports:  extractImports(content, ext),
		symbols:  extractSymbols(ctx, pm, relPath, ext, content),
	}
}

// writeParsedFile writes a changed file into the batch transaction.
// Called serially from the batch writer goroutine.
func writeParsedFile(b *store.Batch, pf parsedFile, pending *[]pendingCall, nameIndex map[string][]symbolEntry) error {
	if err := b.ClearFileContents(pf.relPath); err != nil {
		slog.Warn("indexer: clear file contents failed", "path", pf.relPath, "err", err)
	}
	if err := b.UpsertNode(pf.relPath, pf.nodeType, pf.content, pf.hash); err != nil {
		return err
	}
	for _, imp := range pf.imports {
		if err := b.UpsertNode(imp, "import", "", ""); err != nil {
			slog.Warn("indexer: upsert import node failed", "import", imp, "err", err)
		}
		if err := b.UpsertEdge(pf.relPath, imp, "imports"); err != nil {
			slog.Warn("indexer: upsert import edge failed", "from", pf.relPath, "to", imp, "err", err)
		}
	}
	storeSymbols(b, pf.relPath, pf.symbols, pending, nameIndex)
	slog.Debug("indexer: indexed", "path", pf.relPath)
	return nil
}

// extractSymbols dispatches symbol extraction by file extension.
// Pure computation — no DB access. Called from worker goroutines.
func extractSymbols(ctx context.Context, pm *ParserManager, relPath, ext, content string) []Symbol {
	switch ext {
	case ".go":
		return extractGoSymbols(content)
	default:
		lang := extToLang(ext)
		if lang == "" {
			return nil
		}
		if pm == nil {
			return extractSymbolsHeuristic(ext, content)
		}
		jsonStr, err := pm.Parse(ctx, lang, content)
		if err != nil {
			slog.Warn("indexer: parser failed; skipping symbol extraction",
				"path", relPath, "lang", lang, "err", err)
			return nil
		}
		var syms []Symbol
		if err := json.Unmarshal([]byte(jsonStr), &syms); err != nil {
			slog.Warn("indexer: parser returned invalid symbols JSON; skipping symbol extraction",
				"path", relPath, "lang", lang, "err", err)
			return nil
		}
		if len(syms) > 0 {
			slog.Debug("indexer: parser symbols extracted",
				"path", relPath, "lang", lang, "symbols", len(syms))
		}
		return syms
	}
}

// storeSymbols upserts symbol nodes, contains-edges, and calls-edges for a file.
// Same-file calls are written immediately; cross-file callees are appended to
// pending for resolution in pass 2. nameIndex is populated for pass 2 lookup.
func storeSymbols(b *store.Batch, relPath string, syms []Symbol, pending *[]pendingCall, nameIndex map[string][]symbolEntry) {
	callerDir := filepath.Dir(relPath)
	if callerDir == "." {
		callerDir = ""
	}

	// Build a set of symbol names in this file for same-file calls resolution.
	localSyms := make(map[string]bool, len(syms))
	for _, sym := range syms {
		localSyms[sym.Name] = true
	}

	for _, sym := range syms {
		symID := fmt.Sprintf("%s:%s", relPath, sym.Name)
		if err := b.UpsertNode(symID, sym.Type, sym.Content, ""); err != nil {
			slog.Warn("indexer: upsert symbol node failed", "id", symID, "err", err)
		}
		if err := b.UpsertEdge(relPath, symID, "contains"); err != nil {
			slog.Warn("indexer: upsert symbol edge failed", "from", relPath, "to", symID, "err", err)
		}
		// Register in nameIndex for cross-file resolution.
		nameIndex[sym.Name] = append(nameIndex[sym.Name], symbolEntry{id: symID, dir: callerDir})

		for _, callee := range sym.Calls {
			if localSyms[callee] {
				// Same-file call: write edge immediately (target node already stored).
				calleeID := fmt.Sprintf("%s:%s", relPath, callee)
				if err := b.UpsertEdge(symID, calleeID, "calls"); err != nil {
					slog.Warn("indexer: upsert calls edge failed", "from", symID, "to", calleeID, "err", err)
				}
			} else if pending != nil {
				// Cross-file call: defer to pass 2.
				*pending = append(*pending, pendingCall{
					callerID:   symID,
					calleeName: callee,
					callerDir:  callerDir,
				})
			}
		}
	}
}

// cleanupStale removes file nodes that were not seen in the current scan.
func cleanupStale(b *store.Batch, existing []string, seen map[string]bool, relScanRoot string) {
	for _, id := range existing {
		isUnderScanRoot := relScanRoot == "." || strings.HasPrefix(id, relScanRoot+string(filepath.Separator))
		if isUnderScanRoot && !seen[id] {
			if err := b.DeleteNode(id); err != nil {
				slog.Warn("indexer: failed to delete stale node", "path", id, "err", err)
			} else {
				slog.Info("indexer: deleted stale node", "path", id)
			}
		}
	}
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// maxFileBytes is the default file size limit; override with SIEVE_MAX_FILE_BYTES.
var maxFileBytes = env.Int64("SIEVE_MAX_FILE_BYTES", 2*1024*1024)

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

// loadSieveIgnore reads ALLOWED_ROOT/.sieveignore and returns the list of
// patterns. Lines starting with '#' and blank lines are ignored. A trailing
// '/' is stripped so both "foo" and "foo/" match directory name "foo".
func loadSieveIgnore(allowedRoot string) []string {
	data, err := os.ReadFile(filepath.Join(allowedRoot, ".sieveignore"))
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, strings.TrimSuffix(line, "/"))
	}
	return patterns
}

// matchesIgnore returns true when the directory or file name (or its relative
// path from allowedRoot) matches any pattern from .sieveignore.
// Supports exact name matches and filepath.Match glob syntax.
func matchesIgnore(name, relPath string, patterns []string) bool {
	for _, pat := range patterns {
		if matched, _ := filepath.Match(pat, name); matched {
			return true
		}
		if matched, _ := filepath.Match(pat, relPath); matched {
			return true
		}
	}
	return false
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
