package main

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is a re-pass of this project's original durability audit
// (ledger/068-071 in the sibling boat repo) against today's grown schema:
// lease/strategy_id on locks, strategy_id on lock_events, token_hash on
// identities, group_name on strategies, the reclaimed lock_event kind. It
// extends kill_test.go/e2e_test.go's existing patterns rather than
// duplicating them — no SIGKILL-mid-write test here, that's already real
// and covered.

// ── 1. Concurrent fresh-database creation under the grown schema ─────────

// TestConcurrentFreshDatabaseCreationGrownSchema re-runs the exact scenario
// store.go's own doc comments say motivated the retry-on-busy exec
// wrapper (many real OS processes racing migrate() on a brand-new file),
// but against today's much larger CREATE TABLE block: 7 tables, 4
// indexes, and several columns (lease_seconds, renewed_at, strategy_id,
// token_hash, group_name) that didn't exist when that wrapper was first
// proven. Every process runs a different command touching a different
// table, all fired at the same instant against the same nonexistent db
// path, so they race on schema creation itself, not just one row.
func TestConcurrentFreshDatabaseCreationGrownSchema(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "fresh-race.db")

	const n = 24
	type job struct {
		args []string
	}
	jobs := make([]job, n)
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			jobs[i] = job{[]string{"identity", "create", "-db=" + db, "-protect", fmt.Sprintf("-label=racer-ident-%d", i)}}
		case 1:
			jobs[i] = job{[]string{"strategy", "create", "-db=" + db, fmt.Sprintf("-name=racer-strat-%d", i), "-thesis=racing fresh migrate", fmt.Sprintf("-group=race-group-%d", i)}}
		case 2:
			jobs[i] = job{[]string{"lock", "acquire", "-db=" + db, fmt.Sprintf("-resource=racer-res-%d", i), fmt.Sprintf("-identity=racer-lock-%d", i), "-lease=60", "-note=racing fresh migrate"}}
		case 3:
			jobs[i] = job{[]string{"interest", "create", "-db=" + db, fmt.Sprintf("-identity=racer-watch-%d", i), fmt.Sprintf("-resource=racer-res-%d", i)}}
		}
	}

	codes := make([]int, n)
	outs := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := exec.Command(bin, jobs[i].args...).CombinedOutput()
			outs[i] = string(out)
			switch e := err.(type) {
			case nil:
				codes[i] = 0
			case *exec.ExitError:
				codes[i] = e.ExitCode()
			default:
				t.Errorf("job %d: unexpected error running binary: %v", i, err)
				codes[i] = -1
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// None of these 24 commands conflict with each other (every resource,
	// label, and strategy name is unique) — the only thing they share is
	// racing on migrate() for the same brand-new file. Every single one
	// must succeed; any failure here means the grown schema's creation
	// isn't safe under real concurrent-process contention.
	for i, code := range codes {
		if code != 0 {
			t.Errorf("job %d (%v): want exit 0, got %d:\n%s", i, jobs[i].args, code, outs[i])
		}
	}

	// Confirm the schema that actually landed is the real, full, grown
	// one -- not a partial table set from a racer that lost mid-CREATE.
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("opening the raced-into-existence db afterward: %v", err)
	}
	defer s.Close()

	idents, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities after the race: %v", err)
	}
	if len(idents) != n/4 {
		t.Errorf("want %d identities, got %d", n/4, len(idents))
	}
	locks, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks after the race: %v", err)
	}
	if len(locks) != n/4 {
		t.Errorf("want %d locks, got %d", n/4, len(locks))
	}
	for _, l := range locks {
		if l.LeaseSeconds != 60 {
			t.Errorf("lock %s: want lease_seconds=60 (proves the grown lease columns survived the fresh-migrate race), got %d", l.Resource, l.LeaseSeconds)
		}
	}
	strats, err := s.ListStrategies()
	if err != nil {
		t.Fatalf("ListStrategies after the race: %v", err)
	}
	if len(strats) != n/4 {
		t.Errorf("want %d strategies, got %d", n/4, len(strats))
	}
	for _, st := range strats {
		if st.Group == "" {
			t.Errorf("strategy %s: want a non-empty group_name (proves that grown column survived the fresh-migrate race), got empty", st.ID)
		}
	}

	assertIntegrityOK(t, db)
}

// ── 2. WAL/busy-timeout under a new concurrency shape: many goroutines,  ──
// ── one process, one *sql.DB, driven over real HTTP (the serve.go shape) ─

// TestConcurrentHTTPWritesSharedDBHandle is the concurrency pattern the
// original durability pass never exercised, because `serve.go` didn't
// exist yet: many goroutines inside *one* long-lived process, sharing
// *one* *sql.DB connection pool, each triggering a real SQLite write via
// real HTTP requests (net/http's per-request-goroutine model) rather than
// each writer being its own OS process with its own handle. Uses the real
// RemoteStore/newMux pair -- an actual HTTP round trip per write, not a
// direct Go call into the Store.
func TestConcurrentHTTPWritesSharedDBHandle(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()
	rs := newRemoteStore(srv.URL)
	defer rs.Close()

	const n = 60
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Distinct resource per goroutine -- this half of the test
			// is about the pool/handle surviving heavy concurrent
			// *uncontended* write traffic, not lock semantics.
			_, err := rs.AcquireLock(fmt.Sprintf("http-res-%d", i), fmt.Sprintf("http-agent-%d", i), "concurrent http write", "", 0)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: AcquireLock over HTTP against the shared *sql.DB failed: %v", i, err)
		}
	}
	locks, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(locks) != n {
		t.Fatalf("want %d locks landed via %d concurrent HTTP writers sharing one *sql.DB, got %d", n, n, len(locks))
	}

	// Now the contended case, same shape: n goroutines racing over real
	// HTTP for the *same* resource against the same shared handle --
	// TestConcurrentAcquireHasExactlyOneWinner's property, but for the
	// in-process-goroutines-over-HTTP shape instead of separate processes.
	const m = 30
	var wg2 sync.WaitGroup
	codes := make([]int, m)
	start2 := make(chan struct{})
	for i := 0; i < m; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			<-start2
			_, err := rs.AcquireLock("http-contended", fmt.Sprintf("http-contender-%d", i), "", "", 0)
			if err == nil {
				codes[i] = 0
			} else {
				codes[i] = 1
			}
		}(i)
	}
	close(start2)
	wg2.Wait()

	wins := 0
	for _, c := range codes {
		if c == 0 {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 winner among %d goroutines racing over HTTP for one resource against one shared *sql.DB, got %d", m, wins)
	}
}

// ── 3. Disk-full re-test under the grown schema, real ulimit -f ──────────

// TestDiskFullLeaseStrategyLinkedLockFailsCleanly re-runs the original
// pass's ulimit -f disk-full check (ledger/071 in the sibling boat repo:
// "ulimit -f" chosen over hdiutil/loopback tricks for portability, no
// root needed) but against a write that touches more of the grown schema
// than the original's plain lock did: a lease- and strategy-linked
// acquire, which writes strategy_id, lease_seconds, and renewed_at, not
// just the original four columns. Confirms clean failure (a real SQLite
// I/O error, not a panic or hang) and full recovery (no partial row) once
// the constraint is lifted.
func TestDiskFullLeaseStrategyLinkedLockFailsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ulimit -f is a POSIX shell builtin -- this project only targets macOS/Linux (CLAUDE.md)")
	}
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "diskfull.db")

	// Build up a real schema + a real strategy first, unconstrained --
	// the constraint under test is "can't grow the file further," not
	// "can't create it at all."
	run := func(args ...string) (int, string) {
		cmd := exec.Command(bin, append(args, "-db="+db)...)
		out, err := cmd.CombinedOutput()
		switch e := err.(type) {
		case nil:
			return 0, string(out)
		case *exec.ExitError:
			return e.ExitCode(), string(out)
		default:
			t.Fatalf("unexpected error running binary: %v", err)
			return -1, ""
		}
	}
	if code, out := run("strategy", "create", "-name=diskfull-strat", "-thesis=disk-full setup"); code != 0 {
		t.Fatalf("setup strategy create failed: %s", out)
	}
	strategies, out := run("strategy", "list")
	if strategies != 0 {
		t.Fatalf("strategy list failed: %s", out)
	}
	strategyID := strings.Fields(out)[0]

	// Same technique as the original pass: cap the *process's* max file
	// size via the POSIX ulimit -f shell builtin (portable, no root, no
	// mounted volume to clean up), then exec the real binary under it.
	// The db file the setup step above created already exceeds this cap,
	// so any further write requires growing a file beyond the limit and
	// fails immediately -- a real, not simulated, disk-full condition.
	script := `ulimit -f 4 && exec "$1" lock acquire -db="$2" -resource=disk-full-res -identity=disk-full-agent -lease=30 -strategy="$3" -note=disk-full-check`
	cmd := exec.Command("sh", "-c", script, "sh", bin, db, strategyID)
	out2, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("want the lease/strategy-linked acquire to fail cleanly under ulimit -f 4, but it succeeded:\n%s", out2)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("want a clean process exit (not a crash/hang) under disk-full, got: %v", err)
	}
	// isBusyErr's own comment draws this exact line: a disk-I/O error is
	// not a SQLITE_BUSY/"database is locked" condition and correctly
	// isn't retried -- confirm the failure is actually the disk-space one
	// and not, say, a flag-parsing mistake in this test itself.
	if strings.Contains(string(out2), "resource is locked by another identity") {
		t.Fatalf("want a disk-I/O failure, got the unrelated 'already locked' path:\n%s", out2)
	}
	t.Logf("disk-full acquire output (expected failure): %s", out2)

	// Recovery: no partial row was left behind by the failed write. `lock
	// status` always exits 0 (free or held alike) -- the real signal is
	// in the message.
	if code, out := run("lock", "status", "-resource=disk-full-res"); code != 0 || !strings.Contains(out, "disk-full-res: free") {
		t.Fatalf("want disk-full-res to show as free (no partial row survived the failed write), got exit %d:\n%s", code, out)
	}

	// Full recovery: the exact same acquire, unconstrained, now succeeds.
	if code, out := run("lock", "acquire", "-resource=disk-full-res", "-identity=disk-full-agent", "-lease=30", "-strategy="+strategyID, "-note=post-recovery"); code != 0 {
		t.Fatalf("want a clean acquire once the disk-full constraint is lifted, got exit %d:\n%s", code, out)
	}
	assertIntegrityOK(t, db)
}

// ── 4. A genuinely new scenario: sustained mixed real usage, PRAGMA health ──

// TestSustainedMixedWorkloadIntegrityStaysClean is the scenario the
// original pass never covered: hundreds of real operations spread across
// every table (locks, identities, interests, propagations, strategies,
// strategy_events, lock_events), not one table hammered in isolation.
// Confirms PRAGMA integrity_check reports clean after real, heavy, mixed
// use -- the kind of workload a single-table-focused pass wouldn't catch
// an issue in.
func TestSustainedMixedWorkloadIntegrityStaysClean(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mixed-workload.db")
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}

	const rounds = 250
	var identities []string
	var strategies []string
	for r := 0; r < rounds; r++ {
		ident, err := s.CreateIdentity(fmt.Sprintf("mixed-agent-%d", r))
		if err != nil {
			t.Fatalf("round %d: CreateIdentity: %v", r, err)
		}
		identities = append(identities, ident.ID)

		strat, err := s.CreateStrategy(fmt.Sprintf("mixed-strat-%d", r), "sustained mixed workload thesis")
		if err != nil {
			t.Fatalf("round %d: CreateStrategy: %v", r, err)
		}
		strategies = append(strategies, strat.ID)
		if r%3 == 0 {
			if err := s.SetStrategyGroup(strat.ID, fmt.Sprintf("mixed-group-%d", r%5)); err != nil {
				t.Fatalf("round %d: SetStrategyGroup: %v", r, err)
			}
		}

		resource := fmt.Sprintf("mixed-res-%d", r)
		in, err := s.CreateInterest(ident.ID, resource, "watching")
		if err != nil {
			t.Fatalf("round %d: CreateInterest: %v", r, err)
		}

		lease := 0
		if r%2 == 0 {
			lease = 120
		}
		if _, err := s.AcquireLock(resource, ident.ID, "mixed workload acquire", strat.ID, lease); err != nil {
			t.Fatalf("round %d: AcquireLock: %v", r, err)
		}
		if _, err := s.LogStrategyEvent(strat.ID, ident.ID, "step_started", "began mixed workload round"); err != nil {
			t.Fatalf("round %d: LogStrategyEvent(step_started): %v", r, err)
		}
		if _, err := s.LogStrategyEvent(strat.ID, ident.ID, "finding", "a finding from the mixed workload"); err != nil {
			t.Fatalf("round %d: LogStrategyEvent(finding): %v", r, err)
		}

		// A second identity contends the same resource -- exercises the
		// "denied" lock_event kind and, via the interest above, a real
		// propagation row too.
		other, err := s.CreateIdentity(fmt.Sprintf("mixed-contender-%d", r))
		if err != nil {
			t.Fatalf("round %d: CreateIdentity(contender): %v", r, err)
		}
		if _, err := s.AcquireLock(resource, other.ID, "contending", "", 0); err == nil {
			t.Fatalf("round %d: contended acquire unexpectedly succeeded", r)
		}

		deliveries, err := s.ListPropagationsForIdentity(ident.ID, true)
		if err != nil {
			t.Fatalf("round %d: ListPropagationsForIdentity: %v", r, err)
		}
		for _, d := range deliveries {
			if err := s.AcknowledgePropagation(d.ID); err != nil {
				t.Fatalf("round %d: AcknowledgePropagation: %v", r, err)
			}
		}
		_ = in

		if r%4 == 0 {
			if err := s.ReleaseLock(resource, ident.ID, "releasing mixed workload lock", false); err != nil {
				t.Fatalf("round %d: ReleaseLock: %v", r, err)
			}
		}
		if r%7 == 0 {
			if err := s.CloseStrategy(strat.ID, ident.ID, "closed as part of mixed workload"); err != nil {
				t.Fatalf("round %d: CloseStrategy: %v", r, err)
			}
		}
	}

	locks, err := s.ListLocks()
	if err != nil {
		t.Fatalf("final ListLocks: %v", err)
	}
	t.Logf("mixed workload finished: %d rounds, %d identities, %d strategies, %d locks still held", rounds, len(identities), len(strategies), len(locks))

	if err := s.Close(); err != nil {
		t.Fatalf("closing store before integrity check: %v", err)
	}
	assertIntegrityOK(t, db)
}

// ── 5. Schema-version mismatch: empirical fact-finding, not a fix ────────
//
// README's "Known limitations" already names this gap honestly ("No
// schema-version check... keep the binary in sync"). These two tests
// don't try to close it (out of scope, no migration/versioning framework
// -- this project deliberately has neither); they pin down, empirically,
// exactly what happens today in both mismatch directions, discovered by
// hand first (see this session's report) and encoded here so the finding
// stays true.

// legacySchemaDDL is store.go's migrate() as it existed at commit 222cf89
// -- the last commit before group_name (strategies), strategy_id
// (locks/lock_events), lease_seconds/renewed_at (locks), and token_hash
// (identities) were added. A real, historical "old binary" schema, not an
// invented one; copied by hand rather than re-deriving it because pinning
// an exact historical byte-for-byte DDL is the actual point of this test.
const legacySchemaDDL = `
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
	kind               TEXT NOT NULL,
	identity_id        TEXT NOT NULL,
	holder_identity_id TEXT,
	note               TEXT,
	forced             INTEGER NOT NULL DEFAULT 0,
	ts                 INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS interests (
	id          TEXT PRIMARY KEY,
	identity_id TEXT NOT NULL,
	resource    TEXT NOT NULL,
	status      TEXT NOT NULL,
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
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	closed_at  INTEGER
);

CREATE TABLE IF NOT EXISTS strategy_events (
	id          TEXT PRIMARY KEY,
	strategy_id TEXT NOT NULL,
	kind        TEXT NOT NULL,
	identity_id TEXT NOT NULL,
	note        TEXT NOT NULL,
	ts          INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_interests_resource   ON interests(resource);
CREATE INDEX IF NOT EXISTS idx_propagations_identity ON propagations(identity_id);
CREATE INDEX IF NOT EXISTS idx_lock_events_resource  ON lock_events(resource);
CREATE INDEX IF NOT EXISTS idx_strategy_events_strategy ON strategy_events(strategy_id);
`

// TestSchemaMismatchNewBinaryAgainstOldSchema is empirical fact-finding
// for the "old database, new binary" direction. Finding, confirmed by
// hand first with a real binary built from commit 222cf89: today's
// migrate() is CREATE TABLE IF NOT EXISTS only -- it never ALTERs an
// existing table -- so opening a legacy-schema file with today's code
// leaves the new columns permanently missing, and every current INSERT/
// SELECT in this codebase unconditionally names them. Net effect: this
// direction fails LOUD, immediately, and every time -- a clear Go error
// surfaces on essentially the first read or write, never silent
// corruption or a partial write.
func TestSchemaMismatchNewBinaryAgainstOldSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("opening raw legacy db: %v", err)
	}
	if _, err := raw.Exec(legacySchemaDDL); err != nil {
		t.Fatalf("creating legacy schema: %v", err)
	}
	// Seed one legacy-shaped row directly, old-schema style (no
	// strategy_id/lease_seconds/renewed_at columns exist to populate).
	if _, err := raw.Exec(`INSERT INTO identities (id, label, created_at) VALUES ('ident-legacy', 'legacy-agent', ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seeding legacy identity: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO locks (resource, holder_identity_id, note, acquired_at) VALUES ('legacy-res', 'ident-legacy', 'pre-existing', ?)`, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seeding legacy lock: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing raw legacy db: %v", err)
	}

	// Today's real openLocalStore/migrate() against that file: migrate()
	// itself must not fail (CREATE TABLE IF NOT EXISTS is a true no-op on
	// tables that already exist -- confirmed, not assumed).
	s, err := openLocalStore(dbPath)
	if err != nil {
		t.Fatalf("today's openLocalStore against a legacy-schema db: want a clean open (migrate() should no-op on existing tables), got: %v", err)
	}
	defer s.Close()

	// Reads against the legacy locks table fail loudly -- scanLock always
	// selects strategy_id/lease_seconds/renewed_at, which don't exist.
	if _, err := s.ListLocks(); err == nil {
		t.Fatalf("want ListLocks against a legacy locks table to fail cleanly (it selects columns that don't exist there), got no error")
	} else if !isSchemaMismatchSQLError(err) {
		t.Fatalf("want a clear schema-mismatch SQL error, got: %v", err)
	}

	// Writes fail loudly too -- CreateIdentity's INSERT always names
	// token_hash, which doesn't exist in the legacy identities table.
	if _, err := s.CreateIdentity("new-binary-agent"); err == nil {
		t.Fatalf("want CreateIdentity against a legacy identities table to fail cleanly (no token_hash column there), got no error")
	} else if !isSchemaMismatchSQLError(err) {
		t.Fatalf("want a clear schema-mismatch SQL error, got: %v", err)
	}

	// AcquireLock (the Store method itself -- VerifyIdentityToken is a
	// separate call the CLI/HTTP layer makes first, not part of this
	// method) fails at its own INSERT, which always names strategy_id/
	// lease_seconds/renewed_at.
	if _, err := s.AcquireLock("legacy-res-2", "brand-new-identity", "", "", 0); err == nil {
		t.Fatalf("want AcquireLock against a legacy-schema db to fail cleanly, got no error")
	} else if !isSchemaMismatchSQLError(err) {
		t.Fatalf("want a clear schema-mismatch SQL error, got: %v", err)
	}

	// And the CLI's own extra pre-check (VerifyIdentityToken, run before
	// AcquireLock by cmdLockAcquire/locksAcquireHandler) fails the exact
	// same loud way, one step earlier, because it SELECTs token_hash.
	if err := s.VerifyIdentityToken("brand-new-identity", ""); err == nil {
		t.Fatalf("want VerifyIdentityToken against a legacy identities table to fail cleanly (no token_hash column there), got no error")
	} else if !isSchemaMismatchSQLError(err) {
		t.Fatalf("want a clear schema-mismatch SQL error, got: %v", err)
	}
}

// isSchemaMismatchSQLError matches either phrasing SQLite/modernc.org's
// driver uses for "this column doesn't exist here" -- "no such column: x"
// from a SELECT, "table t has no column named x" from an INSERT naming an
// unknown column. Both are the same underlying fact (today's code and
// this file's schema disagree), just worded differently depending on
// which kind of statement hit it.
func isSchemaMismatchSQLError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column named")
}

// TestSchemaMismatchOldCodeIgnoresLeaseReclaimOnNewSchema is empirical
// fact-finding for the reverse direction: "new database, old binary."
// Confirmed by hand first with a real binary built from commit 222cf89
// against a db a current binary had already written lease/strategy
// columns into: the old binary opens and *operates* on the new-schema db
// without error (its SELECTs/INSERTs only ever name its own, smaller
// column list, which SQLite happily allows against a wider table) -- but
// it is completely blind to lease semantics, because that logic didn't
// exist yet in its own source. An expired-lease lock that today's binary
// would correctly reclaim, the old binary instead treats as held forever
// and refuses to touch. This test reproduces that exact blindness with
// hand-written SQL matching the old binary's real queries (see lock.go
// git history at 222cf89), rather than shelling out to a second built
// binary, so it stays a fast, permanent, portable regression check on
// the documented finding instead of a one-off manual repro.
func TestSchemaMismatchOldCodeIgnoresLeaseReclaimOnNewSchema(t *testing.T) {
	s := newTestStore(t)

	// A current binary genuinely acquires a very short lease and lets it
	// expire -- real lease-expiry state, not a fabricated row.
	if _, err := s.AcquireLock("legacy-blind-res", "new-binary-holder", "", "", 1); err != nil {
		t.Fatalf("setup AcquireLock: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	// old binary's exact pre-lease AcquireLock query shape (lock.go at
	// 222cf89): a plain INSERT with ON CONFLICT DO NOTHING naming only
	// the four original columns, no idea lease_seconds/renewed_at even
	// exist, so it can never know to check them.
	res, err := s.db.Exec(
		`INSERT INTO locks (resource, holder_identity_id, note, acquired_at) VALUES (?, ?, ?, ?) ON CONFLICT(resource) DO NOTHING`,
		"legacy-blind-res", "old-binary-contender", "old binary trying to reclaim", time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("old-binary-shaped INSERT against the new-schema locks table: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 0 {
		t.Fatalf("want the old-binary-shaped acquire to find the row already present and insert 0 rows (proving it can't see the lease has expired), got %d rows inserted", n)
	}

	// Meanwhile today's binary, looking at the exact same row, correctly
	// sees the lease has expired and reclaims it -- confirming the gap is
	// real (old binary: permanently stuck) and not just "both agree it's
	// still held, coincidentally."
	l, err := s.AcquireLock("legacy-blind-res", "new-binary-reclaimer", "", "", 0)
	if err != nil {
		t.Fatalf("today's AcquireLock should correctly reclaim the same expired lease the old-binary-shaped query couldn't see: %v", err)
	}
	if !l.Reclaimed {
		t.Fatalf("want Reclaimed=true from today's binary on the same row the old-binary-shaped query treated as permanently held, got false")
	}
}

// ── shared helpers ─────────────────────────────────────────────────────

// assertIntegrityOK runs a real PRAGMA integrity_check against dbPath via
// a fresh connection (not reusing the store's own handle, so this is
// exactly what an operator running `sqlite3 events.db "PRAGMA
// integrity_check"` by hand would see) and fails the test on anything
// but the single clean "ok" row SQLite returns for a healthy file.
func assertIntegrityOK(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening %s for integrity check: %v", dbPath, err)
	}
	defer raw.Close()
	rows, err := raw.Query(`PRAGMA integrity_check`)
	if err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning integrity_check row: %v", err)
		}
		results = append(results, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating integrity_check rows: %v", err)
	}
	if len(results) != 1 || results[0] != "ok" {
		t.Fatalf("PRAGMA integrity_check on %s: want a single clean \"ok\", got %v", dbPath, results)
	}
}
