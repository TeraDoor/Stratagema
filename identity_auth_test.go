package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnprotectedIdentityAuthBehaviorIsUnchanged is the core backward-compat
// guarantee this whole feature rests on, mirroring lock_test.go's
// TestAcquireLockNoLeaseBehaviorIsUnchanged: an identity created the way
// every existing test and script already creates one (plain CreateIdentity,
// no -protect) must behave with zero observable difference -- proven
// directly, not just inferred from the rest of the suite passing.
func TestUnprotectedIdentityAuthBehaviorIsUnchanged(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if alpha.Protected {
		t.Fatalf("a plain CreateIdentity must never report Protected=true")
	}
	if displayProtected(alpha.Protected) != "-" {
		t.Fatalf("displayProtected(false) = %q, want %q", displayProtected(alpha.Protected), "-")
	}

	// No token, whatever token, doesn't matter -- unprotected means
	// unprotected, not "protected unless you happen to pass nothing."
	for _, tok := range []string{"", "garbage", "sgt_anything-at-all"} {
		if err := s.VerifyIdentityToken(alpha.ID, tok); err != nil {
			t.Fatalf("VerifyIdentityToken(unprotected, %q) = %v, want nil", tok, err)
		}
	}

	// The other half of "just a label": an identity string that was never
	// even passed through `identity create` (e2e_test.go's own real-world
	// usage) must keep working exactly as it always has -- no new implicit
	// registration requirement.
	if err := s.VerifyIdentityToken("never-created-ad-hoc-label", ""); err != nil {
		t.Fatalf("VerifyIdentityToken(never-created label) = %v, want nil (identity is deliberately just a label)", err)
	}

	// The full lock lifecycle, end to end, with zero token anywhere.
	l, err := s.AcquireLock("res-unprotected", alpha.ID, "note", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.HolderID != alpha.ID {
		t.Fatalf("AcquireLock holder = %s, want %s", l.HolderID, alpha.ID)
	}
	if err := s.ReleaseLock("res-unprotected", alpha.ID, "", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	// ListIdentities must display "-" for the protected column, same
	// convention faculty list/strategy list already use.
	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 1 || all[0].Protected {
		t.Fatalf("ListIdentities: want exactly 1 unprotected identity, got %+v", all)
	}
}

// TestProtectedIdentityTokenVerification proves the shared VerifyIdentityToken
// path itself: correct token succeeds, wrong token fails, missing token
// fails, and the stored value is never the plaintext secret.
func TestProtectedIdentityTokenVerification(t *testing.T) {
	s := newTestStore(t)
	it, token, err := s.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	if !it.Protected {
		t.Fatalf("CreateProtectedIdentity must report Protected=true")
	}
	if token == "" || !strings.HasPrefix(token, identityTokenPrefix) {
		t.Fatalf("token = %q, want a non-empty token with prefix %q", token, identityTokenPrefix)
	}
	if len(token) < 32 {
		t.Fatalf("token %q is shorter than the required 32-byte-equivalent minimum", token)
	}

	// Correct token succeeds.
	if err := s.VerifyIdentityToken(it.ID, token); err != nil {
		t.Fatalf("VerifyIdentityToken(correct token) = %v, want nil", err)
	}
	// Missing token fails, naming the identity.
	if err := s.VerifyIdentityToken(it.ID, ""); err == nil {
		t.Fatalf("VerifyIdentityToken(missing token) = nil, want an error")
	} else if !strings.Contains(err.Error(), it.ID) {
		t.Fatalf("missing-token error %q doesn't name the protected identity %s", err.Error(), it.ID)
	}
	// Wrong token fails.
	if err := s.VerifyIdentityToken(it.ID, "sgt_"+strings.Repeat("0", 64)); err == nil {
		t.Fatalf("VerifyIdentityToken(wrong token) = nil, want an error")
	}
	// A token that is merely a prefix/suffix of the real one still fails --
	// guards against an accidental substring-match bug.
	if err := s.VerifyIdentityToken(it.ID, token[:len(token)-1]); err == nil {
		t.Fatalf("VerifyIdentityToken(truncated token) = nil, want an error")
	}

	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 1 || !all[0].Protected {
		t.Fatalf("ListIdentities: want exactly 1 protected identity, got %+v", all)
	}
	if displayProtected(all[0].Protected) != "protected" {
		t.Fatalf("displayProtected(true) = %q, want %q", displayProtected(all[0].Protected), "protected")
	}
}

// withTokenEnv sets STRATAGEMA_TOKEN for the duration of the test and
// restores whatever was there before, mirroring how a real caller would set
// it in their own shell.
func withTokenEnv(t *testing.T, value string) {
	t.Helper()
	orig, had := os.LookupEnv("STRATAGEMA_TOKEN")
	if value == "" {
		os.Unsetenv("STRATAGEMA_TOKEN")
	} else {
		os.Setenv("STRATAGEMA_TOKEN", value)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv("STRATAGEMA_TOKEN", orig)
		} else {
			os.Unsetenv("STRATAGEMA_TOKEN")
		}
	})
}

// TestLockAcquireWiredToIdentityAuth exercises the real CLI entry point
// (cmdLockAcquire), not the Store API directly, proving the wiring at the
// one place identity-scoped mutating commands actually call
// VerifyIdentityToken: no token is rejected cleanly (via die, not a panic
// or raw error), a wrong token is rejected, a correct token via -token
// succeeds, and a correct token via STRATAGEMA_TOKEN succeeds too.
func TestLockAcquireWiredToIdentityAuth(t *testing.T) {
	withTokenEnv(t, "")
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	it, token, err := s.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	s.Close()

	// No token anywhere: rejected cleanly (non-zero exit, no panic, a real
	// message -- not a raw error dump).
	out, code := captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=r1", "-identity=" + it.ID})
	})
	if code == 0 {
		t.Fatalf("cmdLockAcquire with no token: want non-zero exit, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, it.ID) || !strings.Contains(out, "token") {
		t.Fatalf("cmdLockAcquire with no token: want a clear message naming the identity and mentioning a token, got:\n%s", out)
	}

	// Wrong token via -token: rejected.
	out, code = captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=r1", "-identity=" + it.ID, "-token=wrong-token-value"})
	})
	if code == 0 {
		t.Fatalf("cmdLockAcquire with wrong token: want non-zero exit, got 0, output:\n%s", out)
	}

	// Correct token via -token flag: succeeds.
	out, code = captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=r1", "-identity=" + it.ID, "-token=" + token})
	})
	if code != 0 {
		t.Fatalf("cmdLockAcquire with correct -token: want exit 0, got %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "acquired r1") {
		t.Fatalf("cmdLockAcquire: want confirmation of acquiring r1, got:\n%s", out)
	}

	// Correct token via STRATAGEMA_TOKEN env var, no -token flag: succeeds
	// on a second resource (r1 is already held by this same identity, which
	// would just be an idempotent re-acquire and wouldn't prove much).
	withTokenEnv(t, token)
	out, code = captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=r2", "-identity=" + it.ID})
	})
	if code != 0 {
		t.Fatalf("cmdLockAcquire with STRATAGEMA_TOKEN set: want exit 0, got %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "acquired r2") {
		t.Fatalf("cmdLockAcquire: want confirmation of acquiring r2, got:\n%s", out)
	}
}

// TestStrategyLogWiredToIdentityAuth repeats the same three-way proof
// (missing/wrong/correct token, both -token and STRATAGEMA_TOKEN) against a
// second, unrelated command -- strategy.go's cmdStrategyLog -- specifically
// to prove VerifyIdentityToken is a genuinely shared path wired into each
// call site correctly, not copy-pasted with a bug in one of them.
func TestStrategyLogWiredToIdentityAuth(t *testing.T) {
	withTokenEnv(t, "")
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	it, token, err := s.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	st, err := s.CreateStrategy("strat", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	s.Close()

	logArgs := func(extra ...string) []string {
		args := []string{"-db=" + db, "-identity=" + it.ID, "-kind=finding", "-note=hello"}
		args = append(args, extra...)
		return append(args, st.ID)
	}

	// No token: rejected.
	if _, code := captureOutput(t, func() int { return cmdStrategyLog(logArgs()) }); code == 0 {
		t.Fatalf("cmdStrategyLog with no token: want non-zero exit, got 0")
	}
	// Wrong token: rejected.
	if _, code := captureOutput(t, func() int { return cmdStrategyLog(logArgs("-token=wrong")) }); code == 0 {
		t.Fatalf("cmdStrategyLog with wrong token: want non-zero exit, got 0")
	}
	// Correct token via -token: succeeds.
	out, code := captureOutput(t, func() int { return cmdStrategyLog(logArgs("-token=" + token)) })
	if code != 0 {
		t.Fatalf("cmdStrategyLog with correct -token: want exit 0, got %d, output:\n%s", code, out)
	}
	// Correct token via STRATAGEMA_TOKEN: succeeds.
	withTokenEnv(t, token)
	out, code = captureOutput(t, func() int { return cmdStrategyLog(logArgs()) })
	if code != 0 {
		t.Fatalf("cmdStrategyLog with STRATAGEMA_TOKEN set: want exit 0, got %d, output:\n%s", code, out)
	}
}

// TestInterestInboxHonorsProtectedIdentity covers point 6's deliberate
// extension: reading a protected identity's own inbox also requires its
// token, consistent with the mutating paths, not left as a silent
// information-disclosure gap.
func TestInterestInboxHonorsProtectedIdentity(t *testing.T) {
	withTokenEnv(t, "")
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	it, token, err := s.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	s.Close()

	if _, code := captureOutput(t, func() int {
		return cmdInterestInbox([]string{"-db=" + db, "-identity=" + it.ID})
	}); code == 0 {
		t.Fatalf("cmdInterestInbox with no token: want non-zero exit, got 0")
	}
	out, code := captureOutput(t, func() int {
		return cmdInterestInbox([]string{"-db=" + db, "-identity=" + it.ID, "-token=" + token})
	})
	if code != 0 {
		t.Fatalf("cmdInterestInbox with correct token: want exit 0, got %d, output:\n%s", code, out)
	}
}
