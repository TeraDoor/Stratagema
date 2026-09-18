package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestLockAndNotifyEndToEnd mirrors the manual dry run this project's
// design was validated with: an uncontested acquire stays quiet, a denied
// acquire and a release both notify a subscribed interest, and the
// resource is free and re-acquirable once released.
func TestLockAndNotifyEndToEnd(t *testing.T) {
	s := newTestStore(t)

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}
	if _, err := s.CreateInterest(beta.ID, "shared-config", "watch it"); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}

	if _, err := s.AcquireLock("shared-config", alpha.ID, "editing pool size", "", 0); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}
	inbox, err := s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("uncontested acquire should not propagate, got %d deliveries", len(inbox))
	}

	if _, err := s.AcquireLock("shared-config", beta.ID, "need to bump timeout", "", 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("AcquireLock(beta) on held resource: want ErrLockHeld, got %v", err)
	}
	inbox, err = s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 1 || inbox[0].Kind != "denied" || inbox[0].Resource != "shared-config" {
		t.Fatalf("want one 'denied' delivery for shared-config, got %+v", inbox)
	}
	if err := s.AcknowledgePropagation(inbox[0].ID); err != nil {
		t.Fatalf("AcknowledgePropagation: %v", err)
	}

	if err := s.ReleaseLock("shared-config", alpha.ID, "pool size bumped, safe to read", false); err != nil {
		t.Fatalf("ReleaseLock(alpha): %v", err)
	}
	inbox, err = s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 1 || inbox[0].Kind != "released" || inbox[0].Note == "" {
		t.Fatalf("want one pending 'released' delivery with a note, got %+v", inbox)
	}

	l, err := s.GetLock("shared-config")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if l != nil {
		t.Fatalf("resource should be free after release, got holder %q", l.HolderID)
	}
	if _, err := s.AcquireLock("shared-config", beta.ID, "bumping timeout", "", 0); err != nil {
		t.Fatalf("AcquireLock(beta) on freed resource: %v", err)
	}
}

// TestDependencyResourceWriteNotifiesWatchersIfAny covers the scenario
// named directly: an agent write-locks a resource other agents' work
// depends on, and only the agents that actually registered an interest
// in it get notified when it changes — an agent that never watched it
// hears nothing, and a second, completely unwatched resource can be
// locked and released with zero propagations and no error, not a crash
// on an empty interest list.
func TestDependencyResourceWriteNotifiesWatchersIfAny(t *testing.T) {
	s := newTestStore(t)

	writer, err := s.CreateIdentity("schema-migrator")
	if err != nil {
		t.Fatalf("CreateIdentity(writer): %v", err)
	}
	watcher, err := s.CreateIdentity("api-layer-agent")
	if err != nil {
		t.Fatalf("CreateIdentity(watcher): %v", err)
	}
	bystander, err := s.CreateIdentity("unrelated-agent")
	if err != nil {
		t.Fatalf("CreateIdentity(bystander): %v", err)
	}

	// api-layer-agent's own work depends on this resource, so it
	// registers a real interest before any write happens.
	if _, err := s.CreateInterest(watcher.ID, "pkg/db/schema.go", "api layer reads the schema, needs to know when it changes"); err != nil {
		t.Fatalf("CreateInterest(watcher): %v", err)
	}

	if _, err := s.AcquireLock("pkg/db/schema.go", writer.ID, "adding a column, will notify on release", "", 0); err != nil {
		t.Fatalf("AcquireLock(writer): %v", err)
	}
	if err := s.ReleaseLock("pkg/db/schema.go", writer.ID, "column added, schema.go changed", false); err != nil {
		t.Fatalf("ReleaseLock(writer): %v", err)
	}

	watcherInbox, err := s.ListPropagationsForIdentity(watcher.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity(watcher): %v", err)
	}
	if len(watcherInbox) != 1 || watcherInbox[0].Kind != "released" || watcherInbox[0].Resource != "pkg/db/schema.go" {
		t.Fatalf("watcher: want one 'released' delivery for pkg/db/schema.go, got %+v", watcherInbox)
	}

	bystanderInbox, err := s.ListPropagationsForIdentity(bystander.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity(bystander): %v", err)
	}
	if len(bystanderInbox) != 0 {
		t.Fatalf("bystander never registered an interest, want zero deliveries, got %+v", bystanderInbox)
	}

	// A second resource nobody ever watched: lock+release must still
	// succeed cleanly, with matchAndPropagate's zero-interests path
	// exercised for real rather than assumed safe.
	if _, err := s.AcquireLock("pkg/cache/evictor.go", writer.ID, "tuning eviction policy", "", 0); err != nil {
		t.Fatalf("AcquireLock(unwatched resource): %v", err)
	}
	if err := s.ReleaseLock("pkg/cache/evictor.go", writer.ID, "eviction policy tuned", false); err != nil {
		t.Fatalf("ReleaseLock(unwatched resource): %v", err)
	}
	for _, id := range []string{writer.ID, watcher.ID, bystander.ID} {
		inbox, err := s.ListPropagationsForIdentity(id, true)
		if err != nil {
			t.Fatalf("ListPropagationsForIdentity(%s) after unwatched release: %v", id, err)
		}
		for _, d := range inbox {
			if d.Resource == "pkg/cache/evictor.go" {
				t.Fatalf("no identity registered interest in pkg/cache/evictor.go, want zero deliveries for it, got %+v", d)
			}
		}
	}
}

func TestReleaseRequiresHolderUnlessForced(t *testing.T) {
	s := newTestStore(t)
	alpha, _ := s.CreateIdentity("agent-alpha")
	beta, _ := s.CreateIdentity("agent-beta")

	if _, err := s.AcquireLock("shared-config", alpha.ID, "", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := s.ReleaseLock("shared-config", beta.ID, "", false); err == nil {
		t.Fatal("non-holder release without -force should fail")
	}
	if err := s.ReleaseLock("shared-config", beta.ID, "taking over", true); err != nil {
		t.Fatalf("forced release: %v", err)
	}
	l, _ := s.GetLock("shared-config")
	if l != nil {
		t.Fatalf("resource should be free after forced release, got holder %q", l.HolderID)
	}
}

func TestReacquireBySameHolderIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	alpha, _ := s.CreateIdentity("agent-alpha")

	first, err := s.AcquireLock("shared-config", alpha.ID, "first note", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	second, err := s.AcquireLock("shared-config", alpha.ID, "updated note", "", 0)
	if err != nil {
		t.Fatalf("re-acquire by same holder should succeed, got %v", err)
	}
	if !second.AcquiredAt.Equal(first.AcquiredAt) {
		t.Fatalf("re-acquire should keep original acquired_at: first=%v second=%v", first.AcquiredAt, second.AcquiredAt)
	}
	if second.Note != "updated note" {
		t.Fatalf("re-acquire should refresh note, got %q", second.Note)
	}
}

// TestPropagationImmuneToClockSkew is the regression test for a real bug
// a durability pass reproduced directly: matchAndPropagate picked the
// lock event to attach to a propagation by sorting on wall-clock ts
// first. A single row written by a process with a badly skewed clock
// (or one crafted maliciously) could permanently hijack every future
// propagation on a resource. rowid — SQLite's own monotonic insert
// order — is immune to this by construction and is now the only sort key.
func TestPropagationImmuneToClockSkew(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.CreateInterest(beta.ID, "res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}

	farFuture := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := s.db.Exec(
		`INSERT INTO lock_events (id, resource, kind, identity_id, holder_identity_id, note, forced, ts)
		 VALUES ('lockevt-fake-future', 'res', 'released', ?, ?, 'FAKE FUTURE EVENT', 0, ?)`,
		alpha.ID, alpha.ID, farFuture,
	); err != nil {
		t.Fatalf("inserting simulated clock-skewed row: %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, "editing", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := s.ReleaseLock("res", alpha.ID, "real release note", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	inbox, err := s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(inbox))
	}
	if inbox[0].Note != "real release note" {
		t.Fatalf("propagation attached to the wrong lock event (clock-skew regression) — got note %q, want the real release's note", inbox[0].Note)
	}
}

// TestCreateInterestIsIdempotentOnRetry is the regression test for a real
// bug a durability pass found: a client that times out and retries an
// identical `interest create` call — the actual "connectivity loss" case
// — silently doubled its own future notifications, since nothing treated
// the retry as the same logical interest.
func TestCreateInterestIsIdempotentOnRetry(t *testing.T) {
	s := newTestStore(t)
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	first, err := s.CreateInterest(beta.ID, "res", "watch it")
	if err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}
	second, err := s.CreateInterest(beta.ID, "res", "watch it")
	if err != nil {
		t.Fatalf("CreateInterest (retry): %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("retrying an identical interest create should return the same interest, got %s then %s", first.ID, second.ID)
	}

	all, err := s.ListInterests()
	if err != nil {
		t.Fatalf("ListInterests: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want exactly 1 interest after a retried create, got %d", len(all))
	}
}

// TestResourceNameRejectsNewlines is the regression test for a real bug a
// durability pass found: a resource name containing an embedded newline
// corrupted `lock list`'s plain-text display once stored. Rejected at
// the write boundary instead.
func TestResourceNameRejectsNewlines(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	if _, err := s.AcquireLock("bad\nname", alpha.ID, "", "", 0); err == nil {
		t.Fatal("AcquireLock should reject a resource name containing a newline")
	}
	if _, err := s.CreateInterest(alpha.ID, "bad\nname", ""); err == nil {
		t.Fatal("CreateInterest should reject a resource name containing a newline")
	}
}

// TestAcquireLockLinksToStrategy is table-driven over the two shapes that
// matter: an empty strategyID leaves the lock unlinked (the backward-compat
// default every pre-existing call site relies on), a real one links it —
// and GetLock/ListLocks both have to agree on the same value.
func TestAcquireLockLinksToStrategy(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "does AcquireLock actually persist the link")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	cases := []struct {
		name       string
		resource   string
		strategyID string
	}{
		{"unlinked", "res-unlinked", ""},
		{"linked", "res-linked", st.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acquired, err := s.AcquireLock(tc.resource, alpha.ID, "note", tc.strategyID, 0)
			if err != nil {
				t.Fatalf("AcquireLock: %v", err)
			}
			if acquired.StrategyID != tc.strategyID {
				t.Fatalf("AcquireLock result: StrategyID = %q, want %q", acquired.StrategyID, tc.strategyID)
			}

			got, err := s.GetLock(tc.resource)
			if err != nil {
				t.Fatalf("GetLock: %v", err)
			}
			if got.StrategyID != tc.strategyID {
				t.Fatalf("GetLock: StrategyID = %q, want %q", got.StrategyID, tc.strategyID)
			}

			all, err := s.ListLocks()
			if err != nil {
				t.Fatalf("ListLocks: %v", err)
			}
			var found bool
			for _, l := range all {
				if l.Resource == tc.resource {
					found = true
					if l.StrategyID != tc.strategyID {
						t.Fatalf("ListLocks: StrategyID = %q, want %q", l.StrategyID, tc.strategyID)
					}
				}
			}
			if !found {
				t.Fatalf("ListLocks: resource %q missing", tc.resource)
			}
		})
	}
}

// TestAcquireLockRejectsUnknownStrategy is the "don't silently accept a
// bogus strategy id" requirement: a strategyID that was never created must
// fail the acquire outright, not land on the lock row unchecked.
func TestAcquireLockRejectsUnknownStrategy(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, "note", "strategy-does-not-exist", 0); err == nil {
		t.Fatal("AcquireLock should reject an unknown strategy id")
	}
	l, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if l != nil {
		t.Fatalf("a rejected acquire should not have created a lock row, got %+v", l)
	}
}

// TestReacquireOverwritesStrategyLink proves the re-acquire-by-same-holder
// path treats strategy_id the same way it already treats note: the new
// call's value wins outright, including clearing a previous link when the
// caller passes "" — a re-acquire states the current truth, it doesn't
// merge with the old one.
func TestReacquireOverwritesStrategyLink(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	first, err := s.AcquireLock("res", alpha.ID, "note", st.ID, 0)
	if err != nil {
		t.Fatalf("AcquireLock(1): %v", err)
	}
	if first.StrategyID != st.ID {
		t.Fatalf("first acquire: StrategyID = %q, want %q", first.StrategyID, st.ID)
	}

	second, err := s.AcquireLock("res", alpha.ID, "note", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock(2): %v", err)
	}
	if second.StrategyID != "" {
		t.Fatalf("re-acquire with empty strategyID should clear the link, got %q", second.StrategyID)
	}
	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.StrategyID != "" {
		t.Fatalf("GetLock after re-acquire: StrategyID = %q, want cleared", got.StrategyID)
	}
}

// TestDeniedAndReleasedEventsCarryHolderStrategyLink is the regression
// proof that lock_events preserves the full history of who locked what for
// which strategy, not just current state: a denied attempt and the eventual
// release both have to record the strategy the *current holder's* lock was
// linked to — not whatever the denied caller happened to ask for — so the
// history stays accurate even when a contending acquire names a different
// (or no) strategy.
func TestDeniedAndReleasedEventsCarryHolderStrategyLink(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	holderStrategy, err := s.CreateStrategy("holder-strategy", "alpha's work")
	if err != nil {
		t.Fatalf("CreateStrategy(holder): %v", err)
	}
	otherStrategy, err := s.CreateStrategy("other-strategy", "beta's unrelated work")
	if err != nil {
		t.Fatalf("CreateStrategy(other): %v", err)
	}

	if _, err := s.AcquireLock("shared-config", alpha.ID, "editing", holderStrategy.ID, 0); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}

	// beta's denied attempt names a different strategy — the recorded
	// event must still carry the holder's (alpha's) link, not beta's.
	if _, err := s.AcquireLock("shared-config", beta.ID, "want it", otherStrategy.ID, 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("AcquireLock(beta): want ErrLockHeld, got %v", err)
	}

	if err := s.ReleaseLock("shared-config", alpha.ID, "done", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	rows, err := s.db.Query(`SELECT kind, identity_id, strategy_id FROM lock_events WHERE resource = 'shared-config' ORDER BY rowid`)
	if err != nil {
		t.Fatalf("query lock_events: %v", err)
	}
	defer rows.Close()
	type row struct {
		kind, identityID, strategyID string
	}
	var got []row
	for rows.Next() {
		var r row
		var strategyID sql.NullString
		if err := rows.Scan(&r.kind, &r.identityID, &strategyID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.strategyID = strategyID.String
		got = append(got, r)
	}
	want := []row{
		{"acquired", alpha.ID, holderStrategy.ID},
		{"denied", beta.ID, holderStrategy.ID},
		{"released", alpha.ID, holderStrategy.ID},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d lock_events, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("lock_events[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// backdateRenewedAt directly rewrites resource's renewed_at, the same
// direct-SQL technique TestPropagationImmuneToClockSkew already uses to
// simulate a specific point in time without a real sleep: acquire a real
// lease, then move its stored renewal into the past so the lazy staleness
// check (Lock.Expired) sees it as already expired against a real clock.
func backdateRenewedAt(t *testing.T, s *Store, resource string, ago time.Duration) {
	t.Helper()
	backdated := time.Now().Add(-ago).UnixMilli()
	res, err := s.db.Exec(`UPDATE locks SET renewed_at = ? WHERE resource = ?`, backdated, resource)
	if err != nil {
		t.Fatalf("backdateRenewedAt: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("backdateRenewedAt: expected to update exactly 1 row, updated %d", n)
	}
}

// TestAcquireLockNoLeaseBehaviorIsUnchanged is the core backward-compat
// guarantee this whole feature rests on: a caller that never mentions a
// lease (leaseSeconds=0, exactly what every pre-existing call site in this
// codebase now passes) gets a lock with no lease fields set at all, that
// never reports itself expired no matter how much time passes -- provably,
// not just by assuming the other tests passing means this one would too.
func TestAcquireLockNoLeaseBehaviorIsUnchanged(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	l, err := s.AcquireLock("res", alpha.ID, "note", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.LeaseSeconds != 0 || l.RenewedAt != nil {
		t.Fatalf("no-lease acquire should leave lease fields unset, got LeaseSeconds=%d RenewedAt=%v", l.LeaseSeconds, l.RenewedAt)
	}
	if l.Reclaimed {
		t.Fatalf("a plain uncontested acquire must never report Reclaimed")
	}

	// Backdate as if a lease had been running for a very long time -- with
	// no lease configured this must never matter, at any distance.
	backdateRenewedAt(t, s, "res", 999*time.Hour)

	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.Expired(time.Now()) {
		t.Fatalf("a lock with no lease configured must never be Expired, regardless of renewed_at")
	}
	if got.leaseState(time.Now()) != "live" {
		t.Fatalf("no-lease lock's leaseState = %q, want %q", got.leaseState(time.Now()), "live")
	}

	all, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(all) != 1 || all[0].Expired(time.Now()) {
		t.Fatalf("ListLocks: want exactly 1 non-expired lock, got %+v", all)
	}
}

// TestAcquireLockWithLeaseLiveThenExpired proves the actual staleness
// arithmetic: a freshly-leased lock reads as live, and the identical lock
// reads as expired once renewed_at+lease_seconds is genuinely in the past
// -- checked lazily by GetLock/ListLocks, never by a background watcher.
func TestAcquireLockWithLeaseLiveThenExpired(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	l, err := s.AcquireLock("res", alpha.ID, "editing", "", 5)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.LeaseSeconds != 5 || l.RenewedAt == nil {
		t.Fatalf("leased acquire should set LeaseSeconds and RenewedAt, got %+v", l)
	}
	if l.Expired(time.Now()) {
		t.Fatalf("a lease just acquired must not read as expired")
	}

	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.leaseState(time.Now()) != "live" {
		t.Fatalf("fresh lease leaseState = %q, want %q", got.leaseState(time.Now()), "live")
	}

	// Move the stored renewal 10s into the past against a 5s lease -- it
	// has genuinely run out.
	backdateRenewedAt(t, s, "res", 10*time.Second)

	expired, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock (after backdate): %v", err)
	}
	if !expired.Expired(time.Now()) {
		t.Fatalf("lease backdated past lease_seconds must read as Expired")
	}
	if expired.leaseState(time.Now()) != "expired" {
		t.Fatalf("expired lease leaseState = %q, want %q", expired.leaseState(time.Now()), "expired")
	}

	all, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(all) != 1 || !all[0].Expired(time.Now()) {
		t.Fatalf("ListLocks must also report the same lock as expired, got %+v", all)
	}
}

// TestRenewLockHeartbeat covers RenewLock's three real outcomes: the
// actual holder renewing a live lease succeeds and pushes renewed_at
// forward, a non-holder is rejected outright, and a lease that already
// expired can't be renewed back to life -- the resource is up for reclaim
// at that point, not silently kept alive by a late heartbeat.
func TestRenewLockHeartbeat(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, "editing", "", 10); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	if _, err := s.RenewLock("res", beta.ID); err == nil {
		t.Fatal("RenewLock by a non-holder should fail")
	}

	renewed, err := s.RenewLock("res", alpha.ID)
	if err != nil {
		t.Fatalf("RenewLock(holder): %v", err)
	}
	if renewed.RenewedAt == nil || renewed.Expired(time.Now()) {
		t.Fatalf("renew by the real holder should produce a live lock, got %+v", renewed)
	}

	backdateRenewedAt(t, s, "res", time.Hour) // well past the 10s lease
	if _, err := s.RenewLock("res", alpha.ID); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("RenewLock after genuine expiry: want ErrLeaseExpired, got %v", err)
	}
}

// TestRenewLockRejectsNoLeaseConfigured is the other named RenewLock
// error: a hold with no lease at all has nothing to renew, and that's a
// real error, not a silent no-op.
func TestRenewLockRejectsNoLeaseConfigured(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "note", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := s.RenewLock("res", alpha.ID); !errors.Is(err, ErrNoLeaseToRenew) {
		t.Fatalf("RenewLock on a no-lease hold: want ErrNoLeaseToRenew, got %v", err)
	}
}

// TestAcquireReclaimsExpiredLeaseWithoutForce is the actual point of the
// whole feature: a second identity can take over a resource whose lease
// has genuinely expired -- no -force required, unlike every other
// take-it-from-someone-else path in this codebase -- and the audit trail
// (lock_events) records this as its own "reclaimed" kind, distinct from
// both a plain "acquired" and a forced "released".
func TestAcquireReclaimsExpiredLeaseWithoutForce(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, "alpha's step", "", 3); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}
	backdateRenewedAt(t, s, "res", time.Hour) // 3s lease, an hour stale: genuinely expired

	claimed, err := s.AcquireLock("res", beta.ID, "beta takes over", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock(beta) on an expired lease without -force: %v", err)
	}
	if claimed.HolderID != beta.ID {
		t.Fatalf("reclaim should hand the resource to the new holder, got holder=%s", claimed.HolderID)
	}
	if !claimed.Reclaimed {
		t.Fatalf("AcquireLock's return value should flag this call as a reclaim")
	}

	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.HolderID != beta.ID {
		t.Fatalf("stored lock row should now be held by beta, got %s", got.HolderID)
	}

	rows, err := s.db.Query(`SELECT kind, identity_id, holder_identity_id, forced FROM lock_events WHERE resource = 'res' ORDER BY rowid`)
	if err != nil {
		t.Fatalf("query lock_events: %v", err)
	}
	defer rows.Close()
	type row struct {
		kind, identityID, holderID string
		forced                     bool
	}
	var events []row
	for rows.Next() {
		var r row
		var forced int
		if err := rows.Scan(&r.kind, &r.identityID, &r.holderID, &forced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.forced = forced != 0
		events = append(events, r)
	}
	want := []row{
		{"acquired", alpha.ID, alpha.ID, false},
		{"reclaimed", beta.ID, alpha.ID, false},
	}
	if len(events) != len(want) {
		t.Fatalf("want %d lock_events, got %d: %+v", len(want), len(events), events)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("lock_events[%d] = %+v, want %+v", i, events[i], w)
		}
	}
}

// TestAcquireDoesNotReclaimALiveLease is the negative case right next to
// the positive one above: a lease that hasn't actually expired yet must
// still be denied like any other held resource, -force or reclaim included
// -- staleness has to be genuine, not assumed from the mere presence of a
// lease.
func TestAcquireDoesNotReclaimALiveLease(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, "alpha's step", "", 300); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}
	if _, err := s.AcquireLock("res", beta.ID, "beta wants it too", "", 0); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("AcquireLock(beta) against a live lease: want ErrLockHeld, got %v", err)
	}
	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.HolderID != alpha.ID {
		t.Fatalf("a live lease must not be reclaimed, holder is now %s", got.HolderID)
	}
}
