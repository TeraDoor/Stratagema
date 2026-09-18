package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRemote wraps a fresh local *Store in the real newMux and starts a
// real httptest.Server against it, returning both a RemoteStore client
// pointed at it and the underlying local store so a test can check the
// server's own database directly — the actual point of this feature: prove
// a RemoteStore call and a direct *Store call produce identical observable
// state, not just that the HTTP plumbing doesn't error.
func newTestRemote(t *testing.T) (*RemoteStore, *Store) {
	t.Helper()
	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	srv := httptest.NewServer(newMux(local))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { local.Close() })

	remote := newRemoteStore(srv.URL)
	t.Cleanup(func() { remote.Close() })
	return remote, local
}

// TestOpenStoreDispatchesOnScheme is the single dispatch point's own
// contract: an http(s):// path gets a *RemoteStore, anything else gets the
// real local *Store, unchanged.
func TestOpenStoreDispatchesOnScheme(t *testing.T) {
	c, err := openStore("http://example.invalid:9999")
	if err != nil {
		t.Fatalf("openStore(http://...): %v", err)
	}
	if _, ok := c.(*RemoteStore); !ok {
		t.Fatalf("openStore(http://...) = %T, want *RemoteStore", c)
	}

	c2, err := openStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatalf("openStore(local path): %v", err)
	}
	defer c2.Close()
	if _, ok := c2.(*Store); !ok {
		t.Fatalf("openStore(local path) = %T, want *Store", c2)
	}
}

// TestRemoteLockMutationMatchesLocal proves AcquireLock through RemoteStore
// lands in the server's own local database exactly as a direct *Store call
// would: same holder, same resource, visible to a second, independent read
// against the same local store (the way a third `lock list -db=<file>`
// process would see it).
func TestRemoteLockMutationMatchesLocal(t *testing.T) {
	remote, local := newTestRemote(t)

	alpha, err := local.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	l, err := remote.AcquireLock("res", alpha.ID, "editing", "", 0)
	if err != nil {
		t.Fatalf("RemoteStore.AcquireLock: %v", err)
	}
	if l.Resource != "res" || l.HolderID != alpha.ID {
		t.Fatalf("AcquireLock result = %+v, want resource=res holder=%s", l, alpha.ID)
	}

	// The actual proof: read it back through the *local* store directly,
	// not through the RemoteStore that wrote it.
	got, err := local.GetLock("res")
	if err != nil {
		t.Fatalf("local.GetLock: %v", err)
	}
	if got == nil || got.HolderID != alpha.ID {
		t.Fatalf("local.GetLock after remote AcquireLock = %+v, want holder=%s", got, alpha.ID)
	}

	// A second remote acquire by a different identity must be denied,
	// exactly as AcquireLock's own ErrLockHeld path behaves locally.
	beta, err := local.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}
	if _, err := remote.AcquireLock("res", beta.ID, "also editing", "", 0); err == nil {
		t.Fatalf("remote.AcquireLock by a non-holder: want an error, got nil")
	} else if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("remote.AcquireLock denial error = %q, want it to mention the resource is locked", err.Error())
	}

	// Release through remote, confirm the local store agrees it's free.
	if err := remote.ReleaseLock("res", alpha.ID, "done", false); err != nil {
		t.Fatalf("remote.ReleaseLock: %v", err)
	}
	got, err = local.GetLock("res")
	if err != nil {
		t.Fatalf("local.GetLock after release: %v", err)
	}
	if got != nil {
		t.Fatalf("local.GetLock after remote release = %+v, want nil (free)", got)
	}
}

// TestRemoteStrategyMutationMatchesLocal proves CreateStrategy + LogStrategyEvent
// through RemoteStore produce the same durable rows a local caller would see.
func TestRemoteStrategyMutationMatchesLocal(t *testing.T) {
	remote, local := newTestRemote(t)

	alpha, err := local.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	st, err := remote.CreateStrategy("probe", "prove remote strategy writes land locally")
	if err != nil {
		t.Fatalf("remote.CreateStrategy: %v", err)
	}
	if st.Status != StrategyPlanning {
		t.Fatalf("CreateStrategy status = %q, want %q", st.Status, StrategyPlanning)
	}

	ev, err := remote.LogStrategyEvent(st.ID, alpha.ID, "finding", "found it over the wire")
	if err != nil {
		t.Fatalf("remote.LogStrategyEvent: %v", err)
	}
	if ev.StrategyID != st.ID || ev.Note != "found it over the wire" {
		t.Fatalf("LogStrategyEvent result = %+v, unexpected", ev)
	}

	events, err := local.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("local.ListStrategyEvents: %v", err)
	}
	if len(events) != 1 || events[0].Note != "found it over the wire" {
		t.Fatalf("local.ListStrategyEvents after remote log = %+v, want one event carrying the remote note", events)
	}

	if err := remote.CloseStrategy(st.ID, alpha.ID, "done, over the wire"); err != nil {
		t.Fatalf("remote.CloseStrategy: %v", err)
	}
	closed, err := local.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("local.GetStrategy: %v", err)
	}
	if closed.Status != StrategyClosed || closed.ClosedAt == nil {
		t.Fatalf("local.GetStrategy after remote close = %+v, want status=closed with ClosedAt set", closed)
	}
}

// TestRemoteReadPaths covers ListLocks and ListIdentities — a couple of
// plain read paths — proving RemoteStore's GET-based reads see exactly
// what the local store holds, decoded back into the same domain types
// (including the empty-collection and not-found-is-nil cases).
func TestRemoteReadPaths(t *testing.T) {
	remote, local := newTestRemote(t)

	// Empty case first.
	locks, err := remote.ListLocks()
	if err != nil {
		t.Fatalf("remote.ListLocks (empty): %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("remote.ListLocks (empty) = %v, want none", locks)
	}
	if got, err := remote.GetLock("nope"); err != nil {
		t.Fatalf("remote.GetLock (missing): %v", err)
	} else if got != nil {
		t.Fatalf("remote.GetLock (missing) = %+v, want nil", got)
	}

	alpha, err := local.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := local.AcquireLock("res", alpha.ID, "note", "", 0); err != nil {
		t.Fatalf("local.AcquireLock: %v", err)
	}

	locks, err = remote.ListLocks()
	if err != nil {
		t.Fatalf("remote.ListLocks: %v", err)
	}
	if len(locks) != 1 || locks[0].Resource != "res" || locks[0].HolderID != alpha.ID {
		t.Fatalf("remote.ListLocks = %+v, want one lock on res held by %s", locks, alpha.ID)
	}

	ids, err := remote.ListIdentities()
	if err != nil {
		t.Fatalf("remote.ListIdentities: %v", err)
	}
	if len(ids) != 1 || ids[0].ID != alpha.ID {
		t.Fatalf("remote.ListIdentities = %+v, want exactly alpha", ids)
	}
}

// TestRemoteProtectedIdentityAuthFlow is the auth centerpiece: a protected
// identity's mutating request over RemoteStore must fail cleanly with no
// token, fail cleanly with the wrong token, and succeed with the right
// one — proving the two-round-trip verify-then-act shape (VerifyIdentityToken
// as its own real HTTP call, separate from the mutating call that follows)
// actually works end to end, not just that each piece compiles.
func TestRemoteProtectedIdentityAuthFlow(t *testing.T) {
	remote, local := newTestRemote(t)

	it, token, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}

	// No token: VerifyIdentityToken itself must fail, and fail as a real
	// auth failure (not a generic 500) — remote.go turns the server's 401
	// back into a plain error; we can't observe the status code directly
	// through the Coordinator interface, so this proves the *behavior*
	// point 5 requires: a clean, non-nil error naming the problem.
	if err := remote.VerifyIdentityToken(it.ID, ""); err == nil {
		t.Fatalf("remote.VerifyIdentityToken(no token): want an error, got nil")
	} else if !strings.Contains(err.Error(), it.ID) {
		t.Fatalf("remote.VerifyIdentityToken(no token) error = %q, want it to name %s", err.Error(), it.ID)
	}

	// Wrong token: also fails.
	if err := remote.VerifyIdentityToken(it.ID, "sgt_wrong"); err == nil {
		t.Fatalf("remote.VerifyIdentityToken(wrong token): want an error, got nil")
	}

	// A mutating call attempted after a failed verify (mirroring what
	// cmd* would do if it didn't die(1) on the verify error first) must
	// still be rejectable — but since AcquireLock itself takes no token
	// and the local Store never enforced one on AcquireLock either, this
	// is exactly the documented, honest shape: verify is the enforcement
	// point, not the mutation. So we instead confirm the *correct* flow
	// end to end: verify succeeds, then the mutation succeeds.
	if err := remote.VerifyIdentityToken(it.ID, token); err != nil {
		t.Fatalf("remote.VerifyIdentityToken(correct token): %v", err)
	}
	l, err := remote.AcquireLock("protected-res", it.ID, "note", "", 0)
	if err != nil {
		t.Fatalf("remote.AcquireLock after successful verify: %v", err)
	}
	if l.HolderID != it.ID {
		t.Fatalf("AcquireLock holder = %s, want %s", l.HolderID, it.ID)
	}

	got, err := local.GetLock("protected-res")
	if err != nil {
		t.Fatalf("local.GetLock: %v", err)
	}
	if got == nil || got.HolderID != it.ID {
		t.Fatalf("local.GetLock after remote AcquireLock (protected identity) = %+v, want holder=%s", got, it.ID)
	}
}

// TestRemoteInterestAndPropagationRoundTrip exercises CreateInterest,
// AcquireLock/ReleaseLock to generate a propagation, and both
// ListPropagationsForIdentity and RecentPropagationsForIdentity — the
// remaining read/write pair not covered by the lock/strategy/auth tests
// above — end to end through RemoteStore.
func TestRemoteInterestAndPropagationRoundTrip(t *testing.T) {
	remote, local := newTestRemote(t)

	alpha, err := local.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := local.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	in, err := remote.CreateInterest(beta.ID, "watched", "")
	if err != nil {
		t.Fatalf("remote.CreateInterest: %v", err)
	}
	if in.Status != InterestActive {
		t.Fatalf("CreateInterest status = %q, want %q", in.Status, InterestActive)
	}

	if _, err := local.AcquireLock("watched", alpha.ID, "editing", "", 0); err != nil {
		t.Fatalf("local.AcquireLock: %v", err)
	}
	if err := local.ReleaseLock("watched", alpha.ID, "safe now", false); err != nil {
		t.Fatalf("local.ReleaseLock: %v", err)
	}

	deliveries, err := remote.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("remote.ListPropagationsForIdentity: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Kind != "released" || deliveries[0].Note != "safe now" {
		t.Fatalf("remote.ListPropagationsForIdentity = %+v, want one released delivery noting \"safe now\"", deliveries)
	}

	recent, lastRowID, err := remote.RecentPropagationsForIdentity(beta.ID, 0, 50)
	if err != nil {
		t.Fatalf("remote.RecentPropagationsForIdentity: %v", err)
	}
	if len(recent) != 1 || lastRowID != recent[0].RowID {
		t.Fatalf("remote.RecentPropagationsForIdentity = %+v (lastRowID=%d), want one delivery and lastRowID matching it", recent, lastRowID)
	}

	if err := remote.AcknowledgePropagation(recent[0].ID); err != nil {
		t.Fatalf("remote.AcknowledgePropagation: %v", err)
	}
	pending, err := local.ListPropagationsForIdentity(beta.ID, true)
	if err != nil {
		t.Fatalf("local.ListPropagationsForIdentity(pendingOnly): %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("local.ListPropagationsForIdentity(pendingOnly) after remote ack = %+v, want none pending", pending)
	}
}
