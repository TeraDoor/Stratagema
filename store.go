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

// Coordinator is the exact method set every CLI command calls on a store —
// formalizing an existing set, not designing a new one: every method below
// was already a *Store method before this interface existed (verified with
// `grep -ohE '\bstore\.[A-Z][A-Za-z]*\(' *.go | grep -v _test.go | sort -u`
// in this repo). *Store satisfies it with zero changes to Store itself.
// RemoteStore (remote.go) is the other implementation, talking to a running
// `stratagema serve` over HTTP/JSON instead of a local SQLite file —
// openStore, below, is the single place that decides which one a caller
// gets, so every cmd* function stays unaware which backend it's actually
// talking to.
type Coordinator interface {
	AcknowledgePropagation(id string) error
	AcquireLock(resource, identityID, note, strategyID string, leaseSeconds int) (*Lock, error)
	Close() error
	CloseStrategy(id, identityID, outcome string) error
	CreateIdentity(label string) (*Identity, error)
	CreateInterest(identityID, resource, label string) (*Interest, error)
	CreateProtectedIdentity(label string) (*Identity, string, error)
	CreateStrategy(name, thesis string) (*Strategy, error)
	GetLock(resource string) (*Lock, error)
	GetStrategy(id string) (*Strategy, error)
	ListIdentities() ([]*Identity, error)
	ListInterests() ([]*Interest, error)
	ListLocks() ([]*Lock, error)
	ListPropagationsForIdentity(identityID string, pendingOnly bool) ([]PropagationDelivery, error)
	ListStrategies() ([]*Strategy, error)
	ListStrategyEvents(strategyID string) ([]*StrategyEvent, error)
	LogStrategyEvent(strategyID, identityID, kind, note string) (*StrategyEvent, error)
	RecentPropagationsForIdentity(identityID string, lastRowID int64, limit int) ([]PropagationDelivery, int64, error)
	RecentStrategyEvents(strategyID string) ([]*StrategyEvent, error)
	ReleaseLock(resource, identityID, note string, force bool) error
	RenewLock(resource, identityID string) (*Lock, error)
	SetInterestStatus(id, status string) error
	SetStrategyGroup(id, group string) error
	SetStrategyStatus(id, status string) error
	VerifyIdentityToken(identityID, token string) error
}

// remoteDBPrefixes are the two schemes that route openStore at a running
// `stratagema serve` instance instead of a local file — anything else is
// treated as a local SQLite path, unchanged.
var remoteDBPrefixes = []string{"http://", "https://"}

func isRemoteDBPath(path string) bool {
	for _, p := range remoteDBPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// openStore is the single dispatch point every cmd* function calls: a
// "-db"/path value that looks like a URL gets a RemoteStore talking HTTP/JSON
// to a running `stratagema serve`; everything else gets exactly today's
// local SQLite path, via openLocalStore, completely unchanged. Everything
// downstream only ever sees the Coordinator interface, so no cmd* function
// needs to know or care which one it got.
func openStore(path string) (Coordinator, error) {
	if isRemoteDBPath(path) {
		return newRemoteStore(path), nil
	}
	return openLocalStore(path)
}

// openLocalStore is today's original openStore, unchanged: it always opens
// a local SQLite file, never dispatches to remote. cmdServe calls this
// directly (the server itself is always the real local store, never remote —
// see remote.go's doc comment), and so does every test that needs the
// concrete *Store type (e.g. to pass to newMux, or to reach into the raw db
// for a durability check).
func openLocalStore(path string) (*Store, error) {
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
	return err
}
