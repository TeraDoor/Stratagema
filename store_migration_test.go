package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOpenStoreAddsColumnsToPreExistingOlderTables is the regression test
// for a real bug, not a hypothetical one: this project's own dogfood
// database (~/stratagema/.stratagema/events.db, first written 2026-09-16)
// predated token_hash/lease/strategy_id/group_name, and every identity/
// lock/strategy command hard-failed against it -- CREATE TABLE IF NOT
// EXISTS is a no-op against an already-existing table, so those columns
// never reached it. This builds an old-shaped database by hand (the exact
// pre-evolution schema, confirmed against the real file before it was
// patched), with a real row in it, then opens it through the normal
// openStore path and checks both that the evolved columns are usable and
// that the pre-existing row survived untouched.
func TestOpenStoreAddsColumnsToPreExistingOlderTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-shaped.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`
CREATE TABLE identities (
	id         TEXT PRIMARY KEY,
	label      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE locks (
	resource           TEXT PRIMARY KEY,
	holder_identity_id TEXT NOT NULL,
	note               TEXT,
	acquired_at        INTEGER NOT NULL
);
CREATE TABLE lock_events (
	id                 TEXT PRIMARY KEY,
	resource           TEXT NOT NULL,
	kind               TEXT NOT NULL,
	identity_id        TEXT NOT NULL,
	holder_identity_id TEXT,
	note               TEXT,
	forced             INTEGER NOT NULL DEFAULT 0,
	ts                 INTEGER NOT NULL
);
CREATE TABLE strategies (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	thesis     TEXT NOT NULL,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	closed_at  INTEGER
);
INSERT INTO identities (id, label, created_at) VALUES ('ident-preexisting', 'agent-from-before-the-schema-grew', 1000);
`); err != nil {
		t.Fatalf("seeding old-shaped schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing seed connection: %v", err)
	}

	s, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore against a pre-existing older-shaped database must not fail, got: %v", err)
	}
	defer s.Close()

	// The pre-existing row must have survived, untouched.
	its, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	found := false
	for _, it := range its {
		if it.ID == "ident-preexisting" {
			found = true
			if it.Label != "agent-from-before-the-schema-grew" {
				t.Fatalf("pre-existing identity's label changed: got %q", it.Label)
			}
			if it.Protected {
				t.Fatalf("a pre-existing identity with no token_hash column at all must read back as unprotected, not Protected=true")
			}
		}
	}
	if !found {
		t.Fatalf("pre-existing identity row was lost by the migration")
	}

	// The evolved column must actually be usable now, not just present.
	if _, _, err := s.CreateProtectedIdentity("agent-created-after-migration"); err != nil {
		t.Fatalf("CreateProtectedIdentity after migration: %v (token_hash column not usable)", err)
	}

	if _, err := s.CreateStrategy("post-migration-strategy", "proving group_name is now writable"); err != nil {
		t.Fatalf("CreateStrategy after migration: %v", err)
	}
}
