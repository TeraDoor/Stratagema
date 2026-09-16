package main

import "testing"

// TestPausedInterestDoesNotReceivePropagations was never verified before —
// pause/resume existed in the CLI and the schema (a status column) but
// nothing confirmed that a paused interest is actually excluded from
// matching, as opposed to just labeled "paused" while still delivering.
func TestPausedInterestDoesNotReceivePropagations(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	in, err := s.CreateInterest(beta.ID, "res", "")
	if err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}

	if err := s.SetInterestStatus(in.ID, InterestPaused); err != nil {
		t.Fatalf("SetInterestStatus(paused): %v", err)
	}

	if _, err := s.AcquireLock("res", alpha.ID, ""); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := s.AcquireLock("res", beta.ID, ""); err == nil {
		t.Fatal("want the acquire to be denied (alpha holds it)")
	}

	inbox, err := s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("a paused interest should not receive propagations, got %d", len(inbox))
	}

	if err := s.SetInterestStatus(in.ID, InterestActive); err != nil {
		t.Fatalf("SetInterestStatus(active): %v", err)
	}
	if err := s.ReleaseLock("res", alpha.ID, "done", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}
	inbox, err = s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("resuming should restore delivery for future events, got %d deliveries", len(inbox))
	}
}

func TestSetInterestStatusUnknownIDErrors(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetInterestStatus("interest-does-not-exist", InterestPaused); err == nil {
		t.Fatal("want an error for an unknown interest id")
	}
}

func TestAcknowledgeUnknownPropagationErrors(t *testing.T) {
	s := newTestStore(t)
	if err := s.AcknowledgePropagation("prop-does-not-exist"); err == nil {
		t.Fatal("want an error for an unknown propagation id")
	}
}

func TestListLocksReturnsAllActiveLocks(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.AcquireLock("res-a", alpha.ID, ""); err != nil {
		t.Fatalf("AcquireLock(res-a): %v", err)
	}
	if _, err := s.AcquireLock("res-b", alpha.ID, ""); err != nil {
		t.Fatalf("AcquireLock(res-b): %v", err)
	}

	locks, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("want 2 active locks, got %d: %+v", len(locks), locks)
	}
}
