package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

// validResourceName rejects a resource name a durability pass proved
// corrupts `lock list`'s plain-text display once it's stored — a name
// containing an embedded newline. Checked at the one or two places a
// resource name is first written (AcquireLock, CreateInterest), not on
// every read, since that's the only point that can keep it out of the
// database in the first place.
func validResourceName(resource string) error {
	if strings.ContainsAny(resource, "\n\r") {
		return fmt.Errorf("resource name must not contain newlines")
	}
	return nil
}

// Lock is the whole coordination primitive: one row per named resource,
// at most one holder at a time. No queueing, no built-in expiry — see the
// doc comments on Acquire/Release for why.
//
// LeaseSeconds/RenewedAt are the opt-in exception, modeled directly on the
// Kubernetes Lease API (and the identical etcd/Chubby/ZooKeeper pattern):
// a lease is never actively watched by anything resident — there is no
// daemon in this project, deliberately (see AcquireLock's doc comment on
// why queueing was rejected for the same reason). Staleness is instead
// checked lazily, by whoever next reads the lock (Expired, below),
// comparing "now" against "renewed_at + lease_seconds." LeaseSeconds == 0
// (equivalently RenewedAt == nil) means no lease was ever requested for
// this hold, and the lock behaves exactly as it always has: it never
// expires.
type Lock struct {
	Resource     string
	HolderID     string
	Note         string
	AcquiredAt   time.Time
	StrategyID   string     // empty = not linked to any strategy
	LeaseSeconds int        // 0 = no lease configured (default, unchanged behavior)
	RenewedAt    *time.Time // nil when LeaseSeconds == 0

	// Reclaimed is set only on the *return value* of AcquireLock/
	// AcquireLockWithLease for the one call that actually took the
	// resource over from a different holder whose lease had genuinely
	// expired. It is not persisted and is always false on anything
	// returned by GetLock/ListLocks — it describes what this call did,
	// not the resource's stored state.
	Reclaimed bool
}

// Expired reports whether l's lease, if it has one, has run past
// renewed_at + lease_seconds as of now. A lock with no lease configured is
// never expired — that is the entire backward-compatibility guarantee for
// a caller that never mentions -lease. This is the one and only place
// staleness is decided, and it is only ever called at read time (GetLock,
// ListLocks, the CLI display paths, AcquireLock's reclaim check) — never
// on a timer.
func (l *Lock) Expired(now time.Time) bool {
	if l.LeaseSeconds <= 0 || l.RenewedAt == nil {
		return false
	}
	// Deliberately millisecond arithmetic (matching reclaimExpiredLock's
	// SQL WHERE clause bit-for-bit: "(? - renewed_at) >= (lease_seconds *
	// 1000)"), not now.Sub(*l.RenewedAt) >= time.Duration(l.LeaseSeconds)*
	// time.Second: time.Duration is nanoseconds in an int64, which
	// overflows once LeaseSeconds*1e9 exceeds ~292 years -- an ordinary Go
	// int, and therefore an ordinary JSON lease_seconds a remote caller can
	// send, easily exceeds that. Confirmed for real: a lease acquired with
	// LeaseSeconds=9_300_000_000 (~294.7 years) read back as already
	// Expired the instant it was acquired. Working in the same
	// milliseconds-since-epoch units the SQL side already uses has no such
	// ceiling for any real lease and, just as importantly, keeps this
	// read-path check and the write-path reclaim condition computing the
	// exact same thing -- switching to whole-seconds precision instead
	// would have fixed the overflow but opened a new, narrower mismatch
	// between the two.
	return now.UnixMilli()-l.RenewedAt.UnixMilli() >= int64(l.LeaseSeconds)*1000
}

// leaseState renders Expired as the two-word vocabulary every display path
// (lock list, lock status, strategy next) uses so they stay consistent:
// "live" for no lease or a lease still within its window, "expired" once
// it's run past renewed_at+lease_seconds. Deliberately not "held"/"free" —
// an expired-lease lock is neither: not "held" (that implies a legitimate
// active holder) and not "free" (something might still come back and
// reclaim it, or renew never fires because nobody's coming).
func (l *Lock) leaseState(now time.Time) string {
	if l.Expired(now) {
		return "expired"
	}
	return "live"
}

// LockEvent is the durable record of every acquire attempt, denial,
// release, and reclaim. It exists mainly to be the thing propagation
// matches against — "denied", "released", and "reclaimed" are the moments
// worth telling a subscriber about; "acquired" is recorded for the audit
// trail (`lock status`) but does not propagate, so a normal, uncontested
// acquire stays silent.
//
// "reclaimed" is its own kind, not a flagged variant of "acquired" and not
// the same thing as a forced release: Forced marks -force's blind,
// unverified override, while a reclaim is the opposite in spirit — a real,
// evidence-based takeover of a lease that demonstrably expired (checked
// against real timestamps by AcquireLock's own reclaim path), so it gets
// its own honest name instead of being lumped in with either.
type LockEvent struct {
	ID         string
	Resource   string
	Kind       string // acquired | denied | released | reclaimed
	IdentityID string
	HolderID   string
	Note       string
	Forced     bool
	Ts         time.Time
	StrategyID string // empty = not linked to any strategy; copied from the lock row at event time
}

var ErrLockHeld = errors.New("resource is locked by another identity")

// ErrNoLeaseToRenew and ErrLeaseExpired are RenewLock's two distinct,
// named failure modes — see RenewLock's doc comment for why renewing a
// no-lease hold and renewing an already-expired one are both real errors,
// not silent no-ops.
var (
	ErrNoLeaseToRenew = errors.New("resource has no lease configured, nothing to renew")
	ErrLeaseExpired   = errors.New("lease already expired: acquire the resource to reclaim it instead of renewing")
)

func scanLock(scan func(dest ...any) error) (*Lock, error) {
	var l Lock
	var note, strategyID sql.NullString
	var acquiredAt int64
	var leaseSeconds, renewedAt sql.NullInt64
	if err := scan(&l.Resource, &l.HolderID, &note, &acquiredAt, &strategyID, &leaseSeconds, &renewedAt); err != nil {
		return nil, err
	}
	l.Note = note.String
	l.AcquiredAt = time.UnixMilli(acquiredAt)
	l.StrategyID = strategyID.String
	if leaseSeconds.Valid {
		l.LeaseSeconds = int(leaseSeconds.Int64)
	}
	if renewedAt.Valid {
		t := time.UnixMilli(renewedAt.Int64)
		l.RenewedAt = &t
	}
	return &l, nil
}

func (s *Store) GetLock(resource string) (*Lock, error) {
	row := s.db.QueryRow(`SELECT resource, holder_identity_id, note, acquired_at, strategy_id, lease_seconds, renewed_at FROM locks WHERE resource = ?`, resource)
	l, err := scanLock(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) ListLocks() ([]*Lock, error) {
	rows, err := s.db.Query(`SELECT resource, holder_identity_id, note, acquired_at, strategy_id, lease_seconds, renewed_at FROM locks ORDER BY acquired_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Lock
	for rows.Next() {
		l, err := scanLock(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// recordLockEvent's strategyID is the link to carry onto the event row, not
// necessarily the one the caller just typed — AcquireLock passes its own
// param for "acquired", but "denied" and "released" pass the *existing*
// lock row's StrategyID, since those events describe the hold that was
// already in place, not whatever the current caller asked for.
func (s *Store) recordLockEvent(resource, kind, identityID, holderID, note, strategyID string, forced bool) (string, error) {
	id, err := newID("lockevt")
	if err != nil {
		return "", err
	}
	f := 0
	if forced {
		f = 1
	}
	_, err = s.exec(
		`INSERT INTO lock_events (id, resource, kind, identity_id, holder_identity_id, note, forced, ts, strategy_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, resource, kind, identityID, holderID, note, f, time.Now().UnixMilli(), strategyID,
	)
	return id, err
}

// AcquireLock claims resource for identityID. Fail-fast, always: if it's
// already held by someone else with a live (or no) lease, this returns
// ErrLockHeld immediately — there is no wait/queue/retry built in,
// deliberately, because honoring a queue would require something running
// in the background to service it, and this project has no forever-running
// worker. An agent that wants to wait polls or subscribes via `interest`
// instead.
//
// strategyID is optional (empty = unlinked) and, when non-empty, must name
// a strategy that actually exists — checked up front so a typo'd id can
// never land silently on a lock row, the same "reject, don't accept
// garbage" posture GetStrategy's callers already follow elsewhere.
//
// leaseSeconds is optional too (0 = no lease, every existing call site's
// default and today's exact no-expiry behavior, unchanged). A positive
// value is the opt-in Kubernetes-Lease-API-style mechanism this file adds:
// renewed_at is stamped to now on this acquire (and on every subsequent
// `lock renew`); staleness is never watched by anything resident — see the
// Lock doc comment — only checked lazily, by whoever next reads the lock.
//
// Re-acquiring your own already-held lock is idempotent (refreshes the
// note, keeps the original acquired_at) rather than an error — an agent
// re-running a step against a resource it still holds is a normal case,
// not a conflict. The refresh also overwrites strategy_id and the lease
// unconditionally, same as it already does for note: a re-acquire call
// states the current truth, not a delta, so passing "" / leaseSeconds=0
// here deliberately clears a previous link/lease rather than leaving a
// stale one behind. Notably, this path runs even if the caller's own
// existing lease had already expired — nothing has reclaimed it yet, so
// the original holder simply taking the step again is not a conflict.
//
// A lock held by a *different* identity whose lease has genuinely expired
// is reclaimable right here, without -force: see reclaimExpiredLock. This
// is the actual point of the lease mechanism — a real, evidence-based
// takeover (the lease demonstrably ran out, checked against real
// timestamps) is categorically different from -force's blind override, so
// it doesn't require it.
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
// The reclaim path (below) gets the same treatment via a conditioned
// UPDATE.
func (s *Store) AcquireLock(resource, identityID, note, strategyID string, leaseSeconds int) (*Lock, error) {
	if err := validResourceName(resource); err != nil {
		return nil, err
	}
	// cmdLockAcquire rejects a negative -lease before this is ever called,
	// but that's a CLI-only screen: any other caller of Store's Go API can
	// call AcquireLock directly with no equivalent check, passing
	// leaseSeconds:-1 straight through. Without this, leaseColumns' own
	// "<=0 means no lease" rule would silently downgrade that to an
	// unbounded, never-expiring lock instead of reporting the caller's
	// mistake -- a materially different, and wrong, outcome. Enforced here
	// so every entry point gets it, not just the CLI's.
	if leaseSeconds < 0 {
		return nil, fmt.Errorf("lock acquire: lease must not be negative")
	}
	if strategyID != "" {
		st, err := s.GetStrategy(strategyID)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("lock acquire: strategy %s not found", strategyID)
		}
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	leaseArg, renewedArg := leaseColumns(leaseSeconds, nowMs)

	// Bounded retry covers two vanishingly rare windows: the resource is
	// released/reclaimed between our failed claim below and the GetLock
	// that finds out why it failed, or a reclaim attempt (below) loses a
	// race with a concurrent reclaimer — not a contention backoff either
	// way.
	for attempt := 0; attempt < 3; attempt++ {
		res, err := s.exec(
			`INSERT INTO locks (resource, holder_identity_id, note, acquired_at, strategy_id, lease_seconds, renewed_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(resource) DO NOTHING`,
			resource, identityID, note, nowMs, strategyID, leaseArg, renewedArg,
		)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if _, err := s.recordLockEvent(resource, "acquired", identityID, identityID, note, strategyID, false); err != nil {
				return nil, err
			}
			return newAcquiredLock(resource, identityID, note, strategyID, nowMs, nowMs, leaseSeconds, false), nil
		}

		existing, err := s.GetLock(resource)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			continue // freed between our failed insert and this read — retry the claim
		}
		if existing.HolderID == identityID {
			if _, err := s.exec(
				`UPDATE locks SET note = ?, strategy_id = ?, lease_seconds = ?, renewed_at = ? WHERE resource = ?`,
				note, strategyID, leaseArg, renewedArg, resource,
			); err != nil {
				return nil, err
			}
			return newAcquiredLock(resource, identityID, note, strategyID, existing.AcquiredAt.UnixMilli(), nowMs, leaseSeconds, false), nil
		}

		if existing.Expired(now) {
			claimed, err := s.reclaimExpiredLock(resource, identityID, note, strategyID, existing, nowMs, leaseArg, renewedArg, leaseSeconds)
			if err != nil {
				return nil, err
			}
			if claimed != nil {
				return claimed, nil
			}
			continue // lost the race to reclaim (someone else got there first) — retry from scratch
		}

		if _, err := s.recordLockEvent(resource, "denied", identityID, existing.HolderID, note, existing.StrategyID, false); err != nil {
			return nil, err
		}
		if err := s.matchAndPropagate(resource); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s holds %q since %s", ErrLockHeld, existing.HolderID, resource, existing.AcquiredAt.UTC().Format(time.RFC3339))
	}
	return nil, fmt.Errorf("lock acquire: too much contention on %q, try again", resource)
}

// leaseColumns turns a Go leaseSeconds int into the (possibly-NULL) pair of
// driver args the locks table's two nullable columns expect: leaseSeconds
// <= 0 means "no lease," which must land as real SQL NULL in both columns,
// not zero — a stored 0 would (wrongly) mean "immediately expired."
func leaseColumns(leaseSeconds int, nowMs int64) (leaseArg, renewedArg any) {
	if leaseSeconds <= 0 {
		return nil, nil
	}
	return leaseSeconds, nowMs
}

// newAcquiredLock builds the in-memory Lock a successful acquire/reacquire/
// reclaim returns. acquiredAtMs and renewedAtMs are passed separately
// because they diverge on a re-acquire: acquired_at is preserved from the
// original hold, but renewed_at always refreshes to now.
func newAcquiredLock(resource, identityID, note, strategyID string, acquiredAtMs, renewedAtMs int64, leaseSeconds int, reclaimed bool) *Lock {
	l := &Lock{
		Resource:     resource,
		HolderID:     identityID,
		Note:         note,
		AcquiredAt:   time.UnixMilli(acquiredAtMs),
		StrategyID:   strategyID,
		LeaseSeconds: leaseSeconds,
		Reclaimed:    reclaimed,
	}
	if leaseSeconds > 0 {
		t := time.UnixMilli(renewedAtMs)
		l.RenewedAt = &t
	}
	return l
}

// reclaimExpiredLock takes resource over from prior's holder, whose lease
// has genuinely run out, without requiring -force — the actual point of a
// lease. The UPDATE's WHERE clause re-checks the same staleness condition
// at write time, not just at the snapshot read in acquireLock above,
// closing the same TOCTOU race class AcquireLock's ON CONFLICT and
// ReleaseLock's conditioned DELETE already guard against (S057): two
// concurrent reclaimers must not both believe they won. Returns (nil, nil)
// — not an error — when the race is lost (someone else reclaimed, the
// original holder renewed, or released it first), so the caller retries
// the whole decision from scratch.
//
// Recorded as its own "reclaimed" lock_event, never "acquired": the audit
// trail should show a takeover-from-an-expired-lease as the distinct,
// evidence-based thing it is, not conflate it with an uncontested claim or
// with a forced override (Forced stays false here — see LockEvent's doc
// comment on why). HolderID on the event names who it was reclaimed from.
func (s *Store) reclaimExpiredLock(resource, identityID, note, strategyID string, prior *Lock, nowMs int64, leaseArg, renewedArg any, leaseSeconds int) (*Lock, error) {
	res, err := s.exec(
		`UPDATE locks SET holder_identity_id = ?, note = ?, acquired_at = ?, strategy_id = ?, lease_seconds = ?, renewed_at = ?
		 WHERE resource = ? AND holder_identity_id = ? AND lease_seconds IS NOT NULL AND renewed_at IS NOT NULL
		   AND (? - renewed_at) >= (lease_seconds * 1000)`,
		identityID, note, nowMs, strategyID, leaseArg, renewedArg,
		resource, prior.HolderID, nowMs,
	)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	if _, err := s.recordLockEvent(resource, "reclaimed", identityID, prior.HolderID, note, strategyID, false); err != nil {
		return nil, err
	}
	if err := s.matchAndPropagate(resource); err != nil {
		return nil, err
	}
	return newAcquiredLock(resource, identityID, note, strategyID, nowMs, nowMs, leaseSeconds, true), nil
}

// RenewLock is the lease heartbeat, and the only reason a lease-holding
// caller ever needs to touch the database again before it's done: it
// re-stamps renewed_at to now so the lazy staleness check in Expired keeps
// treating the hold as live. Only the current holder may renew — same
// ownership check ReleaseLock already uses, not reinvented.
//
// Renewing a lock with no lease configured is a real, named error
// (ErrNoLeaseToRenew) — there's nothing to renew, this isn't "start a
// lease retroactively." Renewing after the lease has already expired is
// also a real, named error (ErrLeaseExpired) rather than a silent
// resurrection: a real lease-based system doesn't let a late heartbeat
// retroactively un-expire it. The resource is up for reclaim like any
// other stale lease (AcquireLock's reclaim path) — the (former) holder's
// own recourse at that point is to acquire it again, not renew it.
func (s *Store) RenewLock(resource, identityID string) (*Lock, error) {
	existing, err := s.GetLock(resource)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("resource %q is not locked", resource)
	}
	if existing.HolderID != identityID {
		return nil, fmt.Errorf("resource %q is held by %s, not %s", resource, existing.HolderID, identityID)
	}
	if existing.LeaseSeconds <= 0 {
		return nil, ErrNoLeaseToRenew
	}
	now := time.Now()
	if existing.Expired(now) {
		return nil, ErrLeaseExpired
	}

	nowMs := now.UnixMilli()
	// Same TOCTOU discipline as AcquireLock/ReleaseLock: the WHERE clause
	// re-checks holder and liveness at write time, not just at the
	// snapshot read above, so a renew racing a reclaim can't silently
	// "win" against a reclaimer that got there first.
	res, err := s.exec(
		`UPDATE locks SET renewed_at = ? WHERE resource = ? AND holder_identity_id = ?
		   AND lease_seconds IS NOT NULL AND renewed_at IS NOT NULL
		   AND (? - renewed_at) < (lease_seconds * 1000)`,
		nowMs, resource, identityID, nowMs,
	)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("resource %q's lease expired or changed hands before the renew completed", resource)
	}
	t := time.UnixMilli(nowMs)
	return &Lock{
		Resource:     existing.Resource,
		HolderID:     existing.HolderID,
		Note:         existing.Note,
		AcquiredAt:   existing.AcquiredAt,
		StrategyID:   existing.StrategyID,
		LeaseSeconds: existing.LeaseSeconds,
		RenewedAt:    &t,
	}, nil
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

	// Same fix as AcquireLock's TOCTOU race (S057): the check above reads
	// a snapshot, so the delete itself has to be the single atomic
	// operation that decides the real outcome, not a second statement
	// trusting that snapshot is still true. Only the caller whose DELETE
	// actually removes a row goes on to record the event and propagate —
	// closes a real gap where two concurrent releases of the same lock
	// (e.g. two overlapping -force calls) could otherwise both "succeed"
	// and double-notify every subscriber for one real state change.
	var res sql.Result
	if force {
		res, err = s.exec(`DELETE FROM locks WHERE resource = ?`, resource)
	} else {
		res, err = s.exec(`DELETE FROM locks WHERE resource = ? AND holder_identity_id = ?`, resource, identityID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if force {
			// Someone else already freed it in the gap above — the
			// intent ("this resource should be free") is already
			// satisfied. Idempotent, not an error.
			return nil
		}
		now, gerr := s.GetLock(resource)
		if gerr != nil {
			return gerr
		}
		if now == nil {
			return fmt.Errorf("resource %q was already released (by someone else) before this call completed", resource)
		}
		return fmt.Errorf("resource %q is now held by %s, not %s (use -force to override)", resource, now.HolderID, identityID)
	}

	if _, err := s.recordLockEvent(resource, "released", identityID, existing.HolderID, note, existing.StrategyID, existing.HolderID != identityID); err != nil {
		return err
	}
	return s.matchAndPropagate(resource)
}

// leaseSuffix is the shared "  lease=Xs/state" display fragment every lock
// listing (lock list, strategy next's two sections) appends — empty string
// for a no-lease lock, so a caller that never mentions -lease sees zero
// output difference from before this feature existed.
func leaseSuffix(l *Lock, now time.Time) string {
	if l.LeaseSeconds <= 0 {
		return ""
	}
	return fmt.Sprintf("  lease=%ds/%s", l.LeaseSeconds, l.leaseState(now))
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdLock(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema lock <acquire|release|status|list|renew> [flags]")
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
	case "renew":
		return cmdLockRenew(rest)
	default:
		return die(2, "lock: unknown subcommand %q (acquire|release|status|list|renew)", sub)
	}
}

func cmdLockAcquire(args []string) int {
	fs := flag.NewFlagSet("lock acquire", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "name of the shared resource to lock (required)")
	identity := fs.String("identity", "", "identity ID acquiring the lock, from `identity create` (required)")
	note := fs.String("note", "", "what you're about to do to it (shown to anyone denied, and to subscribers on release)")
	strategy := fs.String("strategy", "", "strategy ID this acquire belongs to, from `strategy create` (optional — empty means unlinked)")
	lease := fs.Int("lease", 0, "optional lease duration in seconds — omit for the default, no-expiry behavior; when set, the lock is reclaimable by anyone once renewed_at+lease is passed without a `lock renew`")
	token := tokenFlag(fs)
	fs.Parse(args)

	if *resource == "" || *identity == "" {
		return die(1, "lock acquire: -resource and -identity are required")
	}
	if *lease < 0 {
		return die(1, "lock acquire: -lease must not be negative")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock acquire: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "lock acquire: %v", err)
	}

	l, err := store.AcquireLock(*resource, *identity, *note, *strategy, *lease)
	if err != nil {
		return die(1, "lock acquire: %v", err)
	}
	fmt.Printf("acquired %s\n  holder: %s\n  since:  %s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339))
	if l.StrategyID != "" {
		fmt.Printf("  strategy: %s\n", l.StrategyID)
	}
	if l.LeaseSeconds > 0 {
		fmt.Printf("  lease: %ds (renewed now)\n", l.LeaseSeconds)
	}
	if l.Reclaimed {
		fmt.Printf("  reclaimed: previous holder's lease had expired\n")
	}
	return 0
}

func cmdLockRenew(args []string) int {
	fs := flag.NewFlagSet("lock renew", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "name of the leased resource to renew (required)")
	identity := fs.String("identity", "", "identity ID renewing the lock — must be the current holder (required)")
	token := tokenFlag(fs)
	fs.Parse(args)

	if *resource == "" || *identity == "" {
		return die(1, "lock renew: -resource and -identity are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock renew: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "lock renew: %v", err)
	}

	l, err := store.RenewLock(*resource, *identity)
	if err != nil {
		return die(1, "lock renew: %v", err)
	}
	fmt.Printf("renewed %s\n  holder: %s\n  lease:  %ds (renewed now)\n", l.Resource, l.HolderID, l.LeaseSeconds)
	return 0
}

func cmdLockRelease(args []string) int {
	fs := flag.NewFlagSet("lock release", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	resource := fs.String("resource", "", "name of the shared resource to release (required)")
	identity := fs.String("identity", "", "identity ID releasing the lock (required)")
	note := fs.String("note", "", "what changed — delivered to every subscribed interest")
	force := fs.Bool("force", false, "release even if held by a different identity (logged as forced)")
	token := tokenFlag(fs)
	fs.Parse(args)

	if *resource == "" || *identity == "" {
		return die(1, "lock release: -resource and -identity are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "lock release: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "lock release: %v", err)
	}

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
	now := time.Now()
	held := now.Sub(l.AcquiredAt).Round(time.Second)
	if l.Expired(now) {
		// Distinct from "held": an expired-lease lock has no legitimate
		// active holder any more (see Lock.leaseState's doc comment) —
		// say so plainly rather than reporting it the same as a live hold.
		fmt.Printf("%s: expired (lease exceeded) — last held by %s since %s (%s ago)\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), held)
	} else {
		fmt.Printf("%s: held by %s since %s (%s ago)\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), held)
	}
	if l.Note != "" {
		fmt.Printf("  note: %s\n", l.Note)
	}
	if l.StrategyID != "" {
		fmt.Printf("  strategy: %s\n", l.StrategyID)
	}
	if l.LeaseSeconds > 0 {
		renewedAgo := now.Sub(*l.RenewedAt).Round(time.Second)
		fmt.Printf("  lease: %ds, renewed %s ago (%s)\n", l.LeaseSeconds, renewedAgo, l.leaseState(now))
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
	now := time.Now()
	for _, l := range locks {
		strategyField := "strategy=(unlinked)"
		if l.StrategyID != "" {
			strategyField = "strategy=" + l.StrategyID
		}
		fmt.Printf("%-24s  holder=%-20s  since=%s  %s%s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), strategyField, leaseSuffix(l, now))
	}
	return 0
}
