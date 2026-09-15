package main

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// defaultDBPath mirrors substrate's own resolution order, minus the
// substrate.json layer this project deliberately doesn't have: an
// explicit env var, else a project-local default.
func defaultDBPath() string {
	if v := os.Getenv("STRATAGEMA_DB"); v != "" {
		return v
	}
	return filepath.Join(".stratagema", "events.db")
}

func openStore(path string) (*Store, error) {
	if path == "" {
		path = defaultDBPath()
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate is intentionally a flat CREATE TABLE IF NOT EXISTS block, no
// migration framework — this schema is small enough that a version table
// would be more ceremony than the thing it's protecting.
func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS identities (
	id         TEXT PRIMARY KEY,
	label      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS locks (
	resource           TEXT PRIMARY KEY,
	holder_identity_id TEXT NOT NULL,
	note               TEXT,
	acquired_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS lock_events (
	id                 TEXT PRIMARY KEY,
	resource           TEXT NOT NULL,
	kind               TEXT NOT NULL, -- acquired | denied | released
	identity_id        TEXT NOT NULL, -- who performed/attempted this action
	holder_identity_id TEXT,          -- denied: who currently holds it; released: who held it
	note               TEXT,
	forced             INTEGER NOT NULL DEFAULT 0,
	ts                 INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS interests (
	id          TEXT PRIMARY KEY,
	identity_id TEXT NOT NULL,
	resource    TEXT NOT NULL, -- exact match, no wildcard taxonomy
	status      TEXT NOT NULL, -- active | paused
	label       TEXT,
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS propagations (
	id              TEXT PRIMARY KEY,
	interest_id     TEXT NOT NULL,
	lock_event_id   TEXT NOT NULL,
	identity_id     TEXT NOT NULL,
	created_at      INTEGER NOT NULL,
	acknowledged_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_interests_resource   ON interests(resource);
CREATE INDEX IF NOT EXISTS idx_propagations_identity ON propagations(identity_id);
CREATE INDEX IF NOT EXISTS idx_lock_events_resource  ON lock_events(resource);
`)
	return err
}
