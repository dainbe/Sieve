package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrate_FreshDB_SetsVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck

	// Apply schema first (mirrors what New() does).
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentVersion {
		t.Errorf("expected user_version=%d, got %d", currentVersion, version)
	}
}

func TestMigrate_ExistingV0DB_UpgradesToCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck

	// Build a Phase 6-era DB: schema applied, data inserted, no user_version.
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes(id, type, content, hash) VALUES('a.go','go_file','package a','h1')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// user_version is 0 at this point (SQLite default).

	if err := migrate(db); err != nil {
		t.Fatalf("migrate v0 DB: %v", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentVersion {
		t.Errorf("expected user_version=%d, got %d", currentVersion, version)
	}

	// Existing data must survive.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 node after migration, got %d", count)
	}
}

func TestMigrate_AlreadyCurrent_IsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// Pre-stamp to current version.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	// migrate must be a no-op and not error.
	if err := migrate(db); err != nil {
		t.Fatalf("migrate on already-current DB: %v", err)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentVersion {
		t.Errorf("version changed unexpectedly: got %d", version)
	}
}

func TestMigrate_FutureVersion_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// Simulate a newer binary writing user_version = currentVersion + 1.
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("set future user_version: %v", err)
	}

	if err := migrate(db); err == nil {
		t.Fatal("expected error when db version > binary version")
	}
}
