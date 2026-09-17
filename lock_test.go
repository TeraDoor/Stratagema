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
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
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

	if _, err := s.AcquireLock("shared-config", alpha.ID, "editing pool size", ""); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}
	inbox, err := s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("uncontested acquire should not propagate, got %d deliveries", len(inbox))
	}

	if _, err := s.AcquireLock("shared-config", beta.ID, "need to bump timeout", ""); !errors.Is(err, ErrLockHeld) {
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
	if _, err := s.AcquireLock("shared-config", beta.ID, "bumping timeout", ""); err != nil {
		t.Fatalf("AcquireLock(beta) on freed resource: %v", err)
	}
}

func TestReleaseRequiresHolderUnlessForced(t *testing.T) {
	s := newTestStore(t)
	alpha, _ := s.CreateIdentity("agent-alpha")
	beta, _ := s.CreateIdentity("agent-beta")

	if _, err := s.AcquireLock("shared-config", alpha.ID, "", ""); err != nil {
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

	first, err := s.AcquireLock("shared-config", alpha.ID, "first note", "")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	second, err := s.AcquireLock("shared-config", alpha.ID, "updated note", "")
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

	if _, err := s.AcquireLock("res", alpha.ID, "editing", ""); err != nil {
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

	if _, err := s.AcquireLock("bad\nname", alpha.ID, "", ""); err == nil {
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
			acquired, err := s.AcquireLock(tc.resource, alpha.ID, "note", tc.strategyID)
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

	if _, err := s.AcquireLock("res", alpha.ID, "note", "strategy-does-not-exist"); err == nil {
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

	first, err := s.AcquireLock("res", alpha.ID, "note", st.ID)
	if err != nil {
		t.Fatalf("AcquireLock(1): %v", err)
	}
	if first.StrategyID != st.ID {
		t.Fatalf("first acquire: StrategyID = %q, want %q", first.StrategyID, st.ID)
	}

	second, err := s.AcquireLock("res", alpha.ID, "note", "")
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

	if _, err := s.AcquireLock("shared-config", alpha.ID, "editing", holderStrategy.ID); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}

	// beta's denied attempt names a different strategy — the recorded
	// event must still carry the holder's (alpha's) link, not beta's.
	if _, err := s.AcquireLock("shared-config", beta.ID, "want it", otherStrategy.ID); !errors.Is(err, ErrLockHeld) {
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
