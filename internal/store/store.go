package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Store wraps SQLite (FTS5 + knowledge graph).
// All public methods are safe for concurrent use.
// An RWMutex is used: reads are concurrent, writes are exclusive.
// Bulk writes should use WithBatch, which holds the lock for the transaction duration.
type Store struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA temp_store=MEMORY;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS nodes (
	id      TEXT PRIMARY KEY,
	type    TEXT NOT NULL,
	content TEXT NOT NULL,
	hash    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS edges (
	from_id       TEXT NOT NULL,
	to_id         TEXT NOT NULL,
	relation_type TEXT NOT NULL,
	PRIMARY KEY (from_id, to_id, relation_type)
);

CREATE VIRTUAL TABLE IF NOT EXISTS fts_nodes USING fts5(
	id UNINDEXED,
	content,
	content='nodes',
	content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS nodes_ai AFTER INSERT ON nodes BEGIN
	INSERT INTO fts_nodes(rowid, id, content) VALUES (new.rowid, new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS nodes_au AFTER UPDATE ON nodes BEGIN
	INSERT INTO fts_nodes(fts_nodes, rowid, id, content)
		VALUES ('delete', old.rowid, old.id, old.content);
	INSERT INTO fts_nodes(rowid, id, content) VALUES (new.rowid, new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS nodes_ad AFTER DELETE ON nodes BEGIN
	INSERT INTO fts_nodes(fts_nodes, rowid, id, content)
		VALUES ('delete', old.rowid, old.id, old.content);
END;

CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id);
`

func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// Optimize runs SQLite query-plan analysis and a passive WAL checkpoint.
// Safe to call at any time; PRAGMA optimize tracks its own run-time internally.
func (s *Store) Optimize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`PRAGMA analysis_limit=400; PRAGMA optimize`); err != nil {
		return fmt.Errorf("pragma optimize: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}

func (s *Store) Stats() (nodeCount, edgeCount int64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount)
	return
}

func (s *Store) IsHashCurrent(id, hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var stored string
	err := s.db.QueryRow(`SELECT hash FROM nodes WHERE id = ?`, id).Scan(&stored)
	return err == nil && stored == hash
}

// GetAllFileHashes returns id→hash for every node with a non-empty hash (i.e. indexed
// file nodes). Used by parallel indexing workers to skip unchanged files before parsing.
func (s *Store) GetAllFileHashes() (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, hash FROM nodes WHERE hash != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string, 256)
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		m[id] = h
	}
	return m, rows.Err()
}

func (s *Store) Exists(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var dummy string
	return s.db.QueryRow(`SELECT id FROM nodes WHERE id = ?`, id).Scan(&dummy) == nil
}

func (s *Store) UpsertNode(id, nodeType, content, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO nodes(id, type, content, hash) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   type=excluded.type, content=excluded.content, hash=excluded.hash`,
		id, nodeType, content, hash,
	)
	return err
}

func (s *Store) UpsertEdge(fromID, toID, relation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO edges(from_id, to_id, relation_type) VALUES(?,?,?)`,
		fromID, toID, relation,
	)
	return err
}

func (s *Store) DeleteNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, id, id)
	return err
}

// ClearFileContents removes all derived content for a file: outgoing edges
// (imports, contains) from the file node, and all symbol nodes whose ID is
// prefixed with "fileID:". The file node itself is preserved so its hash can
// be updated by a subsequent UpsertNode.
func (s *Store) ClearFileContents(fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM edges WHERE from_id = ?`, fileID); err != nil {
		return err
	}
	// LIKE prefix match for symbol nodes. The fileID is a relative path; '%' and '_'
	// in paths are rare but we escape them via ESCAPE clause for safety.
	prefix := strings.ReplaceAll(strings.ReplaceAll(fileID, `\`, `\\`), `%`, `\%`)
	prefix = strings.ReplaceAll(prefix, `_`, `\_`)
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id LIKE ? ESCAPE '\' AND id != ?`, prefix+":%", fileID)
	return err
}

func (s *Store) GetAllFileNodeIDs() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE type LIKE '%_file'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetFileNodesByPrefix returns file nodes whose IDs start with prefix (exact
// file match or directory prefix match). Uses SQL GLOB for a single index scan.
func (s *Store) GetFileNodesByPrefix(prefix string) ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Match exact file ID or files under directory prefix/.
	// GLOB uses * and ? as wildcards; escape any literal * or ? in the prefix.
	escaped := strings.NewReplacer("*", `\*`, "?", `\?`, `\`, `\\`).Replace(prefix)
	pattern := escaped + "*"
	rows, err := s.db.Query(
		`SELECT id, type, content FROM nodes WHERE (id = ? OR id GLOB ?) AND type LIKE '%_file'`,
		prefix, pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Type, &n.Content); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// GetSymbolCountsByDir returns a map from directory path to symbol node count.
// Symbol nodes are those whose IDs contain ":" (format: "file.go:SymbolName").
func (s *Store) GetSymbolCountsByDir() (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE instr(id, ':') > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	counts := map[string]int{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		sep := strings.IndexByte(id, ':')
		if sep <= 0 {
			continue
		}
		filePath := id[:sep]
		dir := filepath.Dir(filePath)
		if dir == "." {
			dir = "(root)"
		}
		counts[dir]++
	}
	return counts, rows.Err()
}

func (s *Store) FTSSearch(query string, limit int) ([]Node, error) {
	return s.ftsSearch(query, limit, false)
}

// FTSSearchFiles is like FTSSearch but restricts results to file-type nodes
// (type ending in "_file"). Use this for context building so that file-level
// relevance is not dominated by shorter symbol nodes.
func (s *Store) FTSSearchFiles(query string, limit int) ([]Node, error) {
	return s.ftsSearch(query, limit, true)
}

func (s *Store) ftsSearch(query string, limit int, filesOnly bool) ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	safe := sanitizeFTS(query)
	if safe == "" {
		return nil, nil
	}
	return s.runFTSQuery(safe, limit, filesOnly)
}

// runFTSQuery executes a single FTS query without acquiring the store lock.
// Must be called with at least s.mu.RLock held.
func (s *Store) runFTSQuery(safe string, limit int, filesOnly bool) ([]Node, error) {
	// -bm25 converts SQLite's negative rank to a positive score (higher = better).
	var q string
	if filesOnly {
		q = `SELECT n.id, n.type, n.content, -bm25(fts_nodes)
			 FROM fts_nodes f JOIN nodes n ON f.id = n.id
			 WHERE fts_nodes MATCH ? AND n.type LIKE '%_file'
			 ORDER BY rank LIMIT ?`
	} else {
		q = `SELECT n.id, n.type, n.content, -bm25(fts_nodes)
			 FROM fts_nodes f JOIN nodes n ON f.id = n.id
			 WHERE fts_nodes MATCH ?
			 ORDER BY rank LIMIT ?`
	}
	rows, err := s.db.Query(q, safe, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Type, &n.Content, &n.Score); err != nil {
			return nil, err
		}
		n.Content = StripAugment(n.Content)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *Store) GetNode(id string) (Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n Node
	err := s.db.QueryRow(
		`SELECT id, type, content FROM nodes WHERE id = ?`, id,
	).Scan(&n.ID, &n.Type, &n.Content)
	n.Content = StripAugment(n.Content)
	return n, err
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, table := range []string{"edges", "nodes", "term_neighbors", "vectors"} {
		if _, err := s.db.Exec(`DELETE FROM ` + table); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`VACUUM`)
	return err
}

func (s *Store) TraceEdges(startID string, maxDepth int) ([]Edge, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Single recursive CTE replaces the per-node BFS loop, eliminating N+1 queries.
	// UNION (not UNION ALL) deduplicates rows at the SQL level, preventing
	// exponential expansion on cyclic graphs (e.g. mutually-importing files).
	// LIMIT 10000 provides a hard cap for pathological inputs.
	// SQLite supports recursive CTEs since 3.8.3 (2014).
	const q = `
WITH RECURSIVE bfs(from_id, to_id, relation_type, depth) AS (
  SELECT from_id, to_id, relation_type, 1
  FROM edges
  WHERE from_id = ?
  UNION
  SELECT e.from_id, e.to_id, e.relation_type, bfs.depth + 1
  FROM edges e
  JOIN bfs ON e.from_id = bfs.to_id
  WHERE bfs.depth < ?
)
SELECT from_id, to_id, relation_type FROM bfs LIMIT 10000`
	rows, err := s.db.Query(q, startID, maxDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var result []Edge
	seen := map[[2]string]bool{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.FromID, &e.ToID, &e.Relation); err != nil {
			return nil, err
		}
		key := [2]string{e.FromID, e.ToID}
		if !seen[key] {
			seen[key] = true
			result = append(result, e)
		}
	}
	return result, rows.Err()
}

// TraceEdgesMulti performs a multi-seed BFS from all seeds simultaneously,
// returning edges reachable within maxDepth hops and a map of to_id → minimum
// hop count from any seed. This collapses N per-seed TraceEdges calls into one
// SQL round-trip for the builder's BFS expansion.
func (s *Store) TraceEdgesMulti(seeds []string, maxDepth int) (map[string]int, []Edge, error) {
	if len(seeds) == 0 {
		return map[string]int{}, nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	placeholders := strings.Repeat("?,", len(seeds))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(seeds)+1)
	for _, id := range seeds {
		args = append(args, id)
	}
	args = append(args, maxDepth)

	q := `
WITH RECURSIVE bfs(from_id, to_id, relation_type, depth) AS (
  SELECT from_id, to_id, relation_type, 1
  FROM edges
  WHERE from_id IN (` + placeholders + `)
  UNION
  SELECT e.from_id, e.to_id, e.relation_type, bfs.depth + 1
  FROM edges e
  JOIN bfs ON e.from_id = bfs.to_id
  WHERE bfs.depth < ?
)
SELECT from_id, to_id, relation_type, MIN(depth) AS min_depth
FROM bfs
GROUP BY from_id, to_id, relation_type
LIMIT 50000`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close() //nolint:errcheck

	hops := map[string]int{}
	var edges []Edge
	seen := map[[2]string]bool{}
	for rows.Next() {
		var e Edge
		var depth int
		if err := rows.Scan(&e.FromID, &e.ToID, &e.Relation, &depth); err != nil {
			return nil, nil, err
		}
		key := [2]string{e.FromID, e.ToID}
		if !seen[key] {
			seen[key] = true
			edges = append(edges, e)
		}
		// contains edges don't count as hops (symbol within a file = same distance as the file)
		if e.Relation != "contains" {
			if prev, ok := hops[e.ToID]; !ok || depth < prev {
				hops[e.ToID] = depth
			}
		}
	}
	// Second pass: propagate parent hop count through contains edges so symbols
	// within an already-reached file inherit the file's hop distance.
	for _, e := range edges {
		if e.Relation != "contains" {
			continue
		}
		parentHop, ok := hops[e.FromID]
		if !ok {
			continue
		}
		if prev, ok := hops[e.ToID]; !ok || parentHop < prev {
			hops[e.ToID] = parentHop
		}
	}
	return hops, edges, rows.Err()
}

func (s *Store) TraceNodeIDs(startID string, maxDepth int) ([]string, error) {
	edges, err := s.TraceEdges(startID, maxDepth)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{startID: true}
	for _, e := range edges {
		seen[e.ToID] = true
		seen[e.FromID] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

// GetNodesIn fetches nodes for the given IDs in a single query.
// Chunks the request for SQLite's variable limit (999 per query).
func (s *Store) GetNodesIn(ids []string) (map[string]Node, error) {
	if len(ids) == 0 {
		return map[string]Node{}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]Node, len(ids))
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := i + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[i:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		rows, err := s.db.Query(
			`SELECT id, type, content FROM nodes WHERE id IN (`+placeholders+`)`, args...,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n Node
			if err := rows.Scan(&n.ID, &n.Type, &n.Content); err != nil {
				rows.Close() //nolint:errcheck
				return nil, err
			}
			n.Content = StripAugment(n.Content)
			result[n.ID] = n
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Batch provides a transactional write interface for bulk indexing operations.
// Obtain one via Store.WithBatch; do not construct directly.
type Batch struct {
	tx             *sql.Tx
	stmtUpsertNode *sql.Stmt
	stmtUpsertEdge *sql.Stmt
	stmtHashCheck  *sql.Stmt
	stmtClearEdges *sql.Stmt
	stmtClearSyms  *sql.Stmt
	stmtDeleteNode *sql.Stmt
}

// WithBatch runs fn inside a single write transaction, holding the store lock for the
// duration. All writes via Batch are committed atomically on success; rolled back
// on error. Callers must not call Store write methods from inside fn.
func (s *Store) WithBatch(fn func(*Batch) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}

	stmts := [6]*sql.Stmt{}
	queries := [6]string{
		`INSERT INTO nodes(id, type, content, hash) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET type=excluded.type, content=excluded.content, hash=excluded.hash`,
		`INSERT OR IGNORE INTO edges(from_id, to_id, relation_type) VALUES(?,?,?)`,
		`SELECT hash FROM nodes WHERE id = ?`,
		`DELETE FROM edges WHERE from_id = ?`,
		`DELETE FROM nodes WHERE id LIKE ? ESCAPE '\' AND id != ?`,
		`DELETE FROM nodes WHERE id = ?`,
	}
	for i, q := range queries {
		stmts[i], err = tx.Prepare(q)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prepare batch stmt: %w", err)
		}
	}

	b := &Batch{
		tx:             tx,
		stmtUpsertNode: stmts[0],
		stmtUpsertEdge: stmts[1],
		stmtHashCheck:  stmts[2],
		stmtClearEdges: stmts[3],
		stmtClearSyms:  stmts[4],
		stmtDeleteNode: stmts[5],
	}

	if fnErr := fn(b); fnErr != nil {
		_ = tx.Rollback()
		return fnErr
	}
	return tx.Commit()
}

func (b *Batch) UpsertNode(id, nodeType, content, hash string) error {
	_, err := b.stmtUpsertNode.Exec(id, nodeType, content, hash)
	return err
}

func (b *Batch) UpsertEdge(fromID, toID, relation string) error {
	_, err := b.stmtUpsertEdge.Exec(fromID, toID, relation)
	return err
}

func (b *Batch) IsHashCurrent(id, hash string) bool {
	var stored string
	err := b.stmtHashCheck.QueryRow(id).Scan(&stored)
	return err == nil && stored == hash
}

func (b *Batch) ClearFileContents(fileID string) error {
	if _, err := b.stmtClearEdges.Exec(fileID); err != nil {
		return err
	}
	prefix := strings.ReplaceAll(strings.ReplaceAll(fileID, `\`, `\\`), `%`, `\%`)
	prefix = strings.ReplaceAll(prefix, `_`, `\_`)
	_, err := b.stmtClearSyms.Exec(prefix+":%", fileID)
	return err
}

func (b *Batch) DeleteNode(id string) error {
	_, err := b.stmtDeleteNode.Exec(id)
	return err
}

var ftsStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "or": true, "the": true,
	"of": true, "to": true, "in": true, "is": true, "for": true,
	"on": true, "with": true, "by": true, "at": true, "as": true,
	"it": true, "be": true, "do": true, "if": true, "we": true,
}

// TokenizeFTS returns sanitized, stemmed tokens from query suitable for FTS
// coverage matching. Stopwords and single-char tokens are removed; tokens ≥10
// chars are trimmed by 3 (minimum length 5) so that "compression" matches
// "compressFile", "truncation" matches "truncateLines", etc.
func TokenizeFTS(query string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		t = strings.Trim(t, `"'.,;:!?()[]{}/\`)
		if len(t) <= 1 || ftsStopWords[t] || seen[t] {
			continue
		}
		if len(t) >= 10 {
			s := len(t) - 3
			if s < 5 {
				s = 5
			}
			t = t[:s]
		}
		seen[t] = true
		tokens = append(tokens, t)
	}
	return tokens
}

func sanitizeFTS(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	var parts []string
	for _, t := range TokenizeFTS(q) {
		escaped := strings.ReplaceAll(t, `"`, `""`)
		isAlnum := true
		for _, ch := range escaped {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' {
				isAlnum = false
				break
			}
		}
		if isAlnum && len(escaped) >= 3 {
			parts = append(parts, `"`+escaped+`"*`)
		} else if len(escaped) >= 2 {
			parts = append(parts, `"`+escaped+`"`)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

// FTSAugmentSentinel marks the start of the identifier-split augmentation block
// appended to file node content at index time. Everything from this marker to
// end-of-string is stripped before content is returned to callers.
const FTSAugmentSentinel = "\n\n<!--SIEVE_IDX\n"

// StripAugment removes the FTS augmentation block appended at index time.
// If no sentinel is present, s is returned unchanged.
func StripAugment(s string) string {
	if i := strings.Index(s, FTSAugmentSentinel); i >= 0 {
		return s[:i]
	}
	return s
}

type Node struct {
	ID      string
	Type    string
	Content string
	Score   float64 // BM25 relevance score (positive, higher = more relevant); 0 for non-FTS results
}

type Edge struct {
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
	Relation string `json:"relation"`
}

// TermNeighbor is a (term, neighbor, weight) triple for PPMI query expansion.
type TermNeighbor struct {
	Term     string
	Neighbor string
	Weight   float64
}

// GetAllFileNodes returns all file-type nodes with their content.
func (s *Store) GetAllFileNodes() ([]Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, type, content FROM nodes WHERE type LIKE '%_file'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Type, &n.Content); err != nil {
			return nil, err
		}
		n.Content = StripAugment(n.Content)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// ReplaceTermNeighbors atomically replaces all rows in term_neighbors with pairs.
func (s *Store) ReplaceTermNeighbors(pairs []TermNeighbor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM term_neighbors`); err != nil {
		_ = tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO term_neighbors(term, neighbor, weight) VALUES(?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for _, p := range pairs {
		if _, err := stmt.Exec(p.Term, p.Neighbor, p.Weight); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// UpsertVector stores a dense embedding vector for nodeID.
// vec is stored as a little-endian IEEE 754 blob.
func (s *Store) UpsertVector(nodeID string, vec []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO vectors(node_id, dim, vec) VALUES(?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET dim=excluded.dim, vec=excluded.vec`,
		nodeID, len(vec), float32sToBlob(vec),
	)
	return err
}

// LoadAllVectors reads every row from the vectors table and returns a map of
// node_id → embedding. Returns an empty map (not nil) when no vectors exist.
func (s *Store) LoadAllVectors() (map[string][]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT node_id, dim, vec FROM vectors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	result := map[string][]float32{}
	for rows.Next() {
		var nodeID string
		var dim int
		var blob []byte
		if err := rows.Scan(&nodeID, &dim, &blob); err != nil {
			return nil, err
		}
		result[nodeID] = blobToFloat32s(blob, dim)
	}
	return result, rows.Err()
}

// float32sToBlob encodes a float32 slice as a little-endian byte blob.
func float32sToBlob(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// blobToFloat32s decodes a little-endian byte blob into a float32 slice of length dim.
func blobToFloat32s(b []byte, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		if i*4+4 > len(b) {
			break
		}
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// GetTermNeighbors returns up to n neighbors for term, ordered by weight descending.
func (s *Store) GetTermNeighbors(term string, n int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(
		`SELECT neighbor FROM term_neighbors WHERE term = ? ORDER BY weight DESC LIMIT ?`,
		term, n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var neighbors []string
	for rows.Next() {
		var nb string
		if err := rows.Scan(&nb); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, nb)
	}
	return neighbors, rows.Err()
}

// TermNeighborsCount returns the number of rows in term_neighbors.
// Used by BuildPPMI to determine whether the table is populated.
func (s *Store) TermNeighborsCount() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM term_neighbors`).Scan(&count)
	return count, err
}
