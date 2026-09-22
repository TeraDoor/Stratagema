package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// openStore always opens a local SQLite file — the one and only way a
// *Store gets created.
func openStore(path string) (*Store, error) {
	if path == "" {
		path = defaultDBPath()
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// _busy_timeout/_journal_mode are the driver's dedicated DSN keys, not
	// the generic _pragma=name(value) passthrough — real concurrent-writer
	// testing (e2e_test.go) showed the generic form doesn't reliably apply
	// busy_timeout before the first write, producing spurious SQLITE_BUSY
	// under real multi-process contention.
	db, err := sql.Open("sqlite", path+"?_busy_timeout=10000&_journal_mode=WAL")
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

// exec is a drop-in for s.db.Exec that also retries on SQLite's transient
// "database is locked" error. busy_timeout (set in the DSN above) is
// supposed to make this unnecessary, but a real concurrent-process test
// (e2e_test.go, TestConcurrentAcquireHasExactlyOneWinner — many processes
// opening the same brand-new database file at once, which means racing on
// schema creation too, not just on one row) showed it isn't sufficient by
// itself. Every write in this file goes through this instead of s.db.Exec
// directly, migrate() included, because migrate() racing on a fresh file
// is exactly where this was first observed.
func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	var res sql.Result
	var err error
	for attempt := 0; attempt < 50; attempt++ {
		res, err = s.db.Exec(query, args...)
		if err == nil || !isBusyErr(err) {
			return res, err
		}
		time.Sleep(time.Duration(5+attempt) * time.Millisecond)
	}
	return res, err
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// migrate is intentionally a flat CREATE TABLE IF NOT EXISTS block, no
// migration framework — this schema is small enough that a version table
// would be more ceremony than the thing it's protecting.
func (s *Store) migrate() error {
	_, err := s.exec(`
CREATE TABLE IF NOT EXISTS identities (
	id         TEXT PRIMARY KEY,
	label      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	token_hash TEXT -- optional: NULL = unprotected, today's exact behavior (no token ever required). Non-NULL = a sha256 hex digest of the one real secret this identity was given at creation; the secret itself is never stored.
);

CREATE TABLE IF NOT EXISTS locks (
	resource           TEXT PRIMARY KEY,
	holder_identity_id TEXT NOT NULL,
	note               TEXT,
	acquired_at        INTEGER NOT NULL,
	strategy_id        TEXT,   -- optional link to strategies.id; empty/NULL = unlinked
	lease_seconds      INTEGER, -- optional: NULL = no lease, today's exact behavior (never expires)
	renewed_at         INTEGER  -- last renewal timestamp; set on acquire and on every renew; NULL when lease_seconds is NULL
);

CREATE TABLE IF NOT EXISTS lock_events (
	id                 TEXT PRIMARY KEY,
	resource           TEXT NOT NULL,
	kind               TEXT NOT NULL, -- acquired | denied | released | reclaimed
	identity_id        TEXT NOT NULL, -- who performed/attempted this action
	holder_identity_id TEXT,          -- denied: who currently holds it; released: who held it; reclaimed: who it was reclaimed from
	note               TEXT,
	forced             INTEGER NOT NULL DEFAULT 0,
	ts                 INTEGER NOT NULL,
	strategy_id        TEXT -- optional link to strategies.id, copied from the lock row at event time
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

CREATE TABLE IF NOT EXISTS strategies (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	thesis     TEXT NOT NULL,
	status     TEXT NOT NULL, -- planning | active | observing | closed
	created_at INTEGER NOT NULL,
	closed_at  INTEGER,
	group_name TEXT -- optional: several strategies belonging to one larger effort. Named group_name, not group -- group is a reserved word in some SQL contexts.
);

CREATE TABLE IF NOT EXISTS strategy_events (
	id          TEXT PRIMARY KEY,
	strategy_id TEXT NOT NULL,
	kind        TEXT NOT NULL, -- step_started | step_completed | finding | decision | reflection | resource_usage
	identity_id TEXT NOT NULL,
	note        TEXT NOT NULL,
	ts          INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_interests_resource   ON interests(resource);
CREATE INDEX IF NOT EXISTS idx_propagations_identity ON propagations(identity_id);
CREATE INDEX IF NOT EXISTS idx_lock_events_resource  ON lock_events(resource);
CREATE INDEX IF NOT EXISTS idx_strategy_events_strategy ON strategy_events(strategy_id);
`)
	if err != nil {
		return err
	}
	return s.addEvolvedColumns()
}

// addEvolvedColumns closes the one real gap the block above leaves open:
// CREATE TABLE IF NOT EXISTS is a no-op against a table that already
// exists with an older shape, so a column added to this schema since a
// database was first created never actually reaches it. Confirmed live,
// not theoretical -- this project's own real dogfood database (the
// Sep-16 main.go lock coordination) hard-failed on every identity/lock/
// strategy command until patched by hand, because it predated
// token_hash/lease/strategy_id/group_name.
//
// Still no migration framework, no version table -- that was, and
// remains, more ceremony than this schema's size justifies. This is the
// minimum that makes the original reasoning actually hold in practice:
// every column this schema has ever gained, added for real to a table
// that's missing it, a no-op otherwise. New columns get a line added
// here at the same time they're added above.
func (s *Store) addEvolvedColumns() error {
	for _, c := range []struct{ table, column, decl string }{
		{"identities", "token_hash", "TEXT"},
		{"locks", "strategy_id", "TEXT"},
		{"locks", "lease_seconds", "INTEGER"},
		{"locks", "renewed_at", "INTEGER"},
		{"lock_events", "strategy_id", "TEXT"},
		{"strategies", "group_name", "TEXT"},
	} {
		if err := s.addColumnIfMissing(c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(table, column, decl string) error {
	rows, err := s.db.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return err
	}
	defer rows.Close()
	has := rows.Next()
	if err := rows.Err(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = s.exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}
