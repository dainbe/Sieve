package store

import (
	"database/sql"
	"fmt"
)

// currentVersion is the schema version this binary expects.
// Bump this and add a migration function when the schema changes.
const currentVersion = 4

// migrations is an ordered list of upgrade functions.
// migrations[i] upgrades from version i to version i+1.
// Index 0 handles v0 → v1 (baseline: first versioned release).
var migrations = []func(*sql.Tx) error{
	migrateV0toV1,
	migrateV1toV2,
	migrateV2toV3,
	migrateV3toV4,
}

// migrate reads PRAGMA user_version and applies any pending migrations in
// order. Runs inside a dedicated transaction per migration step so a
// partial failure leaves the DB at the last successfully applied version.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version > currentVersion {
		return fmt.Errorf("db schema version %d is newer than binary version %d; upgrade the binary", version, currentVersion)
	}
	for version < currentVersion {
		fn := migrations[version]
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration tx v%d→v%d: %w", version, version+1, err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d→v%d: %w", version, version+1, err)
		}
		// SQLite PRAGMA cannot run inside a regular transaction, but
		// user_version is special: it is stored in the header page and
		// SQLite allows PRAGMA user_version = N inside a transaction.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set user_version %d: %w", version+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d→v%d: %w", version, version+1, err)
		}
		version++
	}
	return nil
}

// migrateV0toV1 is a no-op DDL migration: the schema tables were already
// created by the schema const in New(). This step only establishes that the
// DB has been stamped with a version number (user_version goes 0→1).
func migrateV0toV1(tx *sql.Tx) error {
	return nil
}

// migrateV1toV2 adds the term_neighbors table used by the PPMI query-expansion
// feature. Each row stores a (term, neighbor, weight) triple where weight is
// the PPMI score computed from the indexed corpus.
func migrateV1toV2(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS term_neighbors (
  term     TEXT NOT NULL,
  neighbor TEXT NOT NULL,
  weight   REAL NOT NULL,
  PRIMARY KEY (term, neighbor)
);
CREATE INDEX IF NOT EXISTS idx_term_neighbors_term ON term_neighbors(term);
`)
	return err
}

// migrateV2toV3 clears the content_hash of all file nodes so that the next
// IndexProject run re-processes every file and appends the FTS augmentation
// block (camelCase-split identifier tokens) introduced in Phase 14.
func migrateV2toV3(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE nodes SET hash = '' WHERE type LIKE '%_file'`)
	return err
}

// migrateV3toV4 adds the vectors table for dense semantic retrieval (Phase 18).
// Each row stores one L2-normalized embedding vector for a symbol or file node.
func migrateV3toV4(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS vectors (
    node_id TEXT PRIMARY KEY,
    dim     INTEGER NOT NULL,
    vec     BLOB NOT NULL
)`)
	return err
}
