package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"time"
)

// Lock is the whole coordination primitive: one row per named resource,
// at most one holder at a time. No queueing, no expiry — see the doc
// comments on Acquire/Release for why.
type Lock struct {
	Resource   string
	HolderID   string
	Note       string
	AcquiredAt time.Time
}

// LockEvent is the durable record of every acquire attempt, denial, and
// release. It exists mainly to be the thing propagation matches against —
// "denied" and "released" are the two moments described in the project's
// own design conversation as worth telling a subscriber about; "acquired"
// is recorded for the audit trail (`lock status`) but does not propagate,
// so a normal, uncontested acquire stays silent.
type LockEvent struct {
	ID         string
	Resource   string
	Kind       string // acquired | denied | released
	IdentityID string
	HolderID   string
	Note       string
	Forced     bool
	Ts         time.Time
}

var ErrLockHeld = errors.New("resource is locked by another identity")

func (s *Store) GetLock(resource string) (*Lock, error) {
	row := s.db.QueryRow(`SELECT resource, holder_identity_id, note, acquired_at FROM locks WHERE resource = ?`, resource)
	var l Lock
	var note sql.NullString
	var acquiredAt int64
	err := row.Scan(&l.Resource, &l.HolderID, &note, &acquiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.Note = note.String
	l.AcquiredAt = time.UnixMilli(acquiredAt)
	return &l, nil
}

func (s *Store) ListLocks() ([]*Lock, error) {
	rows, err := s.db.Query(`SELECT resource, holder_identity_id, note, acquired_at FROM locks ORDER BY acquired_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Lock
	for rows.Next() {
		var l Lock
		var note sql.NullString
		var acquiredAt int64
		if err := rows.Scan(&l.Resource, &l.HolderID, &note, &acquiredAt); err != nil {
			return nil, err
		}
		l.Note = note.String
		l.AcquiredAt = time.UnixMilli(acquiredAt)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func (s *Store) recordLockEvent(resource, kind, identityID, holderID, note string, forced bool) (string, error) {
	id, err := newID("lockevt")
	if err != nil {
		return "", err
	}
	f := 0
	if forced {
		f = 1
	}
	_, err = s.exec(
		`INSERT INTO lock_events (id, resource, kind, identity_id, holder_identity_id, note, forced, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, resource, kind, identityID, holderID, note, f, time.Now().UnixMilli(),
	)
	return id, err
}

// AcquireLock claims resource for identityID. Fail-fast, always: if it's
// already held by someone else, this returns ErrLockHeld immediately —
// there is no wait/queue/retry built in, deliberately, because honoring a
// queue would require something running in the background to service it,
// and this project has no forever-running worker. An agent that wants to
// wait polls or subscribes via `interest` instead.
//
// Re-acquiring your own already-held lock is idempotent (refreshes the
// note, keeps the original acquired_at) rather than an error — an agent
// re-running a step against a resource it still holds is a normal case,
// not a conflict.
//
// The claim itself (the INSERT below) has to be a single atomic statement,
// not a read-then-write: a real end-to-end test spawning genuinely
// concurrent OS processes against this exact function (e2e_test.go,
// TestConcurrentAcquireHasExactlyOneWinner) caught a prior version doing
// "SELECT to check, then INSERT if free" — under real contention, two
// processes could both see the resource as free and both proceed to
// INSERT, with the loser hitting a raw SQLite UNIQUE-constraint error
// instead of a clean, recorded "denied" event. `INSERT ... ON CONFLICT DO
// NOTHING` makes the claim itself race-free at the database engine level;
// everything after it only runs once the true outcome is already known.
func (s *Store) AcquireLock(resource, identityID, note string) (*Lock, error) {
	now := time.Now().UnixMilli()

	// Bounded retry only for the vanishingly rare window where the
	// resource is released between our failed claim below and the
	// GetLock that finds out why it failed — not a contention backoff.
	for attempt := 0; attempt < 3; attempt++ {
		res, err := s.exec(
			`INSERT INTO locks (resource, holder_identity_id, note, acquired_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(resource) DO NOTHING`,
			resource, identityID, note, now,
		)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if _, err := s.recordLockEvent(resource, "acquired", identityID, identityID, note, false); err != nil {
				return nil, err
			}
			return &Lock{Resource: resource, HolderID: identityID, Note: note, AcquiredAt: time.UnixMilli(now)}, nil
		}

		existing, err := s.GetLock(resource)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			continue // freed between our failed insert and this read — retry the claim
		}
		if existing.HolderID == identityID {
			if _, err := s.exec(`UPDATE locks SET note = ? WHERE resource = ?`, note, resource); err != nil {
				return nil, err
			}
			return &Lock{Resource: resource, HolderID: identityID, Note: note, AcquiredAt: existing.AcquiredAt}, nil
		}
		if _, err := s.recordLockEvent(resource, "denied", identityID, existing.HolderID, note, false); err != nil {
			return nil, err
		}
		if err := s.matchAndPropagate(resource); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s holds %q since %s", ErrLockHeld, existing.HolderID, resource, existing.AcquiredAt.UTC().Format(time.RFC3339))
	}
	return nil, fmt.Errorf("lock acquire: too much contention on %q, try again", resource)
}

// ReleaseLock frees resource, and is always propagation-worthy — release
// is the actual signal that the resource's shared state changed, which is
// the moment a subscriber needs to hear about, not the acquire. Only the
// current holder can release unless force=true; forcing a release you
// don't hold is the deliberate, logged escape hatch for a stuck lock
// (nothing expires locks automatically — see the Lock doc comment).
func (s *Store) ReleaseLock(resource, identityID, note string, force bool) error {
	existing, err := s.GetLock(resource)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("resource %q is not locked", resource)
	}
	if existing.HolderID != identityID && !force {
		return fmt.Errorf("resource %q is held by %s, not %s (use -force to override)", resource, existing.HolderID, identityID)
	}

	if _, err := s.exec(`DELETE FROM locks WHERE resource = ?`, resource); err != nil {
		return err
	}
	if _, err := s.recordLockEvent(resource, "released", identityID, existing.HolderID, note, existing.HolderID != identityID); err != nil {
		return err
	}
	return s.matchAndPropagate(resource)
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdLock(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema lock <acquire|release|status|list> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "acquire":
		return cmdLockAcquire(rest)
	case "release":
		return cmdLockRelease(rest)
	case "status":
		return cmdLockStatus(rest)
	case "list":
		return cmdLockList(rest)
	default:
		return die(2, "lock: unknown subcommand %q (acquire|release|status|list)", sub)
	}
}

func cmdLockAcquire(args []string) int {
	fs := flag.NewFlagSet("lock acquire", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "name of the shared resource to lock (required)")
	identity := fs.String("identity", "", "identity ID acquiring the lock, from `identity create` (required)")
	note := fs.String("note", "", "what you're about to do to it (shown to anyone denied, and to subscribers on release)")
	fs.Parse(args)

	if *resource == "" || *identity == "" {
		return die(1, "lock acquire: -resource and -identity are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock acquire: %v", err)
	}
	defer store.Close()

	l, err := store.AcquireLock(*resource, *identity, *note)
	if err != nil {
		return die(1, "lock acquire: %v", err)
	}
	fmt.Printf("acquired %s\n  holder: %s\n  since:  %s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339))
	return 0
}

func cmdLockRelease(args []string) int {
	fs := flag.NewFlagSet("lock release", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "name of the shared resource to release (required)")
	identity := fs.String("identity", "", "identity ID releasing the lock (required)")
	note := fs.String("note", "", "what changed — delivered to every subscribed interest")
	force := fs.Bool("force", false, "release even if held by a different identity (logged as forced)")
	fs.Parse(args)

	if *resource == "" || *identity == "" {
		return die(1, "lock release: -resource and -identity are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock release: %v", err)
	}
	defer store.Close()

	if err := store.ReleaseLock(*resource, *identity, *note, *force); err != nil {
		return die(1, "lock release: %v", err)
	}
	fmt.Printf("released %s\n", *resource)
	return 0
}

func cmdLockStatus(args []string) int {
	fs := flag.NewFlagSet("lock status", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "resource to check (required)")
	fs.Parse(args)

	if *resource == "" {
		return die(1, "lock status: -resource is required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock status: %v", err)
	}
	defer store.Close()

	l, err := store.GetLock(*resource)
	if err != nil {
		return die(1, "lock status: %v", err)
	}
	if l == nil {
		fmt.Printf("%s: free\n", *resource)
		return 0
	}
	held := time.Since(l.AcquiredAt).Round(time.Second)
	fmt.Printf("%s: held by %s since %s (%s ago)\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), held)
	if l.Note != "" {
		fmt.Printf("  note: %s\n", l.Note)
	}
	return 0
}

func cmdLockList(args []string) int {
	fs := flag.NewFlagSet("lock list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock list: %v", err)
	}
	defer store.Close()

	locks, err := store.ListLocks()
	if err != nil {
		return die(1, "lock list: %v", err)
	}
	if len(locks) == 0 {
		fmt.Println("no active locks")
		return 0
	}
	for _, l := range locks {
		fmt.Printf("%-24s  holder=%-20s  since=%s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339))
	}
	return 0
}
