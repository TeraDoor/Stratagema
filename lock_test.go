package main

import (
	"errors"
	"path/filepath"
	"testing"
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

	if _, err := s.AcquireLock("shared-config", alpha.ID, "editing pool size"); err != nil {
		t.Fatalf("AcquireLock(alpha): %v", err)
	}
	inbox, err := s.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("ListPropagationsForIdentity: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("uncontested acquire should not propagate, got %d deliveries", len(inbox))
	}

	if _, err := s.AcquireLock("shared-config", beta.ID, "need to bump timeout"); !errors.Is(err, ErrLockHeld) {
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
	if _, err := s.AcquireLock("shared-config", beta.ID, "bumping timeout"); err != nil {
		t.Fatalf("AcquireLock(beta) on freed resource: %v", err)
	}
}

func TestReleaseRequiresHolderUnlessForced(t *testing.T) {
	s := newTestStore(t)
	alpha, _ := s.CreateIdentity("agent-alpha")
	beta, _ := s.CreateIdentity("agent-beta")

	if _, err := s.AcquireLock("shared-config", alpha.ID, ""); err != nil {
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

	first, err := s.AcquireLock("shared-config", alpha.ID, "first note")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	second, err := s.AcquireLock("shared-config", alpha.ID, "updated note")
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
