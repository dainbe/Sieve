package store

import (
	"database/sql"
	"fmt"
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	safe := sanitizeFTS(query)
	if safe == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT n.id, n.type, n.content
		 FROM fts_nodes f JOIN nodes n ON f.id = n.id
		 WHERE fts_nodes MATCH ?
		 ORDER BY rank LIMIT ?`,
		safe, limit,
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

func (s *Store) GetNode(id string) (Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var n Node
	err := s.db.QueryRow(
		`SELECT id, type, content FROM nodes WHERE id = ?`, id,
	).Scan(&n.ID, &n.Type, &n.Content)
	return n, err
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM edges`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM nodes`); err != nil {
		return err
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
		if prev, ok := hops[e.ToID]; !ok || depth < prev {
			hops[e.ToID] = depth
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

func sanitizeFTS(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	tokens := strings.Fields(q)
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.ReplaceAll(t, `"`, `""`)
		quoted = append(quoted, `"`+t+`"`)
	}
	return strings.Join(quoted, " ")
}

type Node struct {
	ID      string
	Type    string
	Content string
}

type Edge struct {
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
	Relation string `json:"relation"`
}
