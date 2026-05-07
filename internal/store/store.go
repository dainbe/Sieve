package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Store wraps SQLite (FTS5 + knowledge graph).
// All public methods are safe for concurrent use.
// An RWMutex is used: reads are concurrent, writes are exclusive.
// Public methods acquire their own locks; callers should not hold Mu while
// calling back into Store methods.
type Store struct {
	Mu   sync.RWMutex // exported so handler can coordinate index vs build
	db   *sql.DB
	path string
}

const schema = `
PRAGMA journal_mode=WAL;
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
	s.Mu.Lock()
	defer s.Mu.Unlock()
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

func (s *Store) Stats() (nodeCount, edgeCount int64, err error) {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount)
	return
}

func (s *Store) IsHashCurrent(id, hash string) bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	var stored string
	err := s.db.QueryRow(`SELECT hash FROM nodes WHERE id = ?`, id).Scan(&stored)
	return err == nil && stored == hash
}

func (s *Store) Exists(id string) bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	var dummy string
	return s.db.QueryRow(`SELECT id FROM nodes WHERE id = ?`, id).Scan(&dummy) == nil
}

func (s *Store) UpsertNode(id, nodeType, content, hash string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO nodes(id, type, content, hash) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   type=excluded.type, content=excluded.content, hash=excluded.hash`,
		id, nodeType, content, hash,
	)
	return err
}

func (s *Store) UpsertEdge(fromID, toID, relation string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO edges(from_id, to_id, relation_type) VALUES(?,?,?)`,
		fromID, toID, relation,
	)
	return err
}

func (s *Store) DeleteNode(id string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, id, id)
	return err
}

func (s *Store) GetAllFileNodeIDs() ([]string, error) {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE type LIKE '%_file'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (s *Store) FTSSearch(query string, limit int) ([]Node, error) {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
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
	defer rows.Close()
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
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	var n Node
	err := s.db.QueryRow(
		`SELECT id, type, content FROM nodes WHERE id = ?`, id,
	).Scan(&n.ID, &n.Type, &n.Content)
	return n, err
}

func (s *Store) Reset() error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
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
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	visited := map[string]bool{startID: true}
	queue := []string{startID}
	var result []Edge
	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var next []string
		for _, id := range queue {
			rows, err := s.db.Query(
				`SELECT to_id, relation_type FROM edges WHERE from_id = ?`, id,
			)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var e Edge
				e.FromID = id
				if err := rows.Scan(&e.ToID, &e.Relation); err != nil {
					rows.Close()
					return nil, err
				}
				result = append(result, e)
				if !visited[e.ToID] {
					visited[e.ToID] = true
					next = append(next, e.ToID)
				}
			}
			rows.Close()
		}
		queue = next
	}
	return result, nil
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
