package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 1. Resource name field-type edge cases ─────────────────────────────────

// TestResourceNameFieldTypeEdgeCases sweeps the field-type space a resource
// name can actually arrive as -- empty, whitespace, several KB long,
// Unicode in its trickiest forms, SQL-meaningful characters, and a
// path-traversal shape -- and proves each one round-trips byte-exact
// through AcquireLock/GetLock/ListLocks rather than being rejected,
// truncated, or silently mangled. validResourceName's own doc comment says
// it rejects exactly one thing (embedded newlines) and nothing else, so
// every case here is expected to succeed.
func TestResourceNameFieldTypeEdgeCases(t *testing.T) {
	longName := strings.Repeat("resource-segment/", 200) + strings.Repeat("x", 4000) // several KB

	cases := []struct {
		name     string
		resource string
	}{
		{"empty string", ""},
		{"whitespace only", "   \t   "},
		{"several KB long", longName},
		{"emoji", "deploy-\U0001F680-pipeline"}, // rocket emoji
		{"RTL text", "תיקיית-עברית-مجلد-عربي"},
		{"combining characters", "é́́-café"}, // combining acutes
		{"single quote", "o'brien's-resource"},
		{"double quote", `say "hi" resource`},
		{"semicolon", "res;DROP"},
		{"sql comment", "res -- comment"},
		{"path traversal", "../../etc/passwd"},
		{"path traversal windows", "..\\..\\Windows\\System32"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			alpha, err := s.CreateIdentity("agent-alpha")
			if err != nil {
				t.Fatalf("CreateIdentity: %v", err)
			}

			acquired, err := s.AcquireLock(tc.resource, alpha.ID, "note", "", 0)
			if err != nil {
				t.Fatalf("AcquireLock(%q): unexpected error: %v", tc.resource, err)
			}
			if acquired.Resource != tc.resource {
				t.Fatalf("AcquireLock result resource = %q, want byte-exact %q", acquired.Resource, tc.resource)
			}

			got, err := s.GetLock(tc.resource)
			if err != nil {
				t.Fatalf("GetLock(%q): %v", tc.resource, err)
			}
			if got == nil {
				t.Fatalf("GetLock(%q): resource not found after acquire", tc.resource)
			}
			if got.Resource != tc.resource {
				t.Fatalf("GetLock resource = %q, want byte-exact %q", got.Resource, tc.resource)
			}

			all, err := s.ListLocks()
			if err != nil {
				t.Fatalf("ListLocks: %v", err)
			}
			if len(all) != 1 || all[0].Resource != tc.resource {
				t.Fatalf("ListLocks = %+v, want exactly one lock for %q", all, tc.resource)
			}

			// The same string must also be safe to use as a lock is held by
			// a different identity -- proves it round-trips through the
			// unique-constraint conflict path too, not just the happy path.
			beta, err := s.CreateIdentity("agent-beta")
			if err != nil {
				t.Fatalf("CreateIdentity(beta): %v", err)
			}
			if _, err := s.AcquireLock(tc.resource, beta.ID, "contending", "", 0); !errors.Is(err, ErrLockHeld) {
				t.Fatalf("AcquireLock(beta) on held resource %q: want ErrLockHeld, got %v", tc.resource, err)
			}
		})
	}
}

// TestResourceNameNewlineVariantsAllRejected proves validResourceName's
// newline rejection actually catches every shape a newline can take --
// bare \n, bare \r, \r\n, and a newline embedded in the middle, at the
// start, or at the end of an otherwise normal name -- at both of the write
// boundaries that call it (AcquireLock and CreateInterest), not just the
// one already-tested "bad\nname" case.
func TestResourceNameNewlineVariantsAllRejected(t *testing.T) {
	cases := []struct {
		name     string
		resource string
	}{
		{"bare LF", "\n"},
		{"bare CR", "\r"},
		{"CRLF", "\r\n"},
		{"LF in middle", "good\nname"},
		{"CR in middle", "good\rname"},
		{"CRLF in middle", "good\r\nname"},
		{"LF at start", "\nname"},
		{"LF at end", "name\n"},
		{"CR at start", "\rname"},
		{"CR at end", "name\r"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			alpha, err := s.CreateIdentity("agent-alpha")
			if err != nil {
				t.Fatalf("CreateIdentity: %v", err)
			}
			if _, err := s.AcquireLock(tc.resource, alpha.ID, "", "", 0); err == nil {
				t.Fatalf("AcquireLock(%q): want rejection, got success", tc.resource)
			}
			if _, err := s.CreateInterest(alpha.ID, tc.resource, ""); err == nil {
				t.Fatalf("CreateInterest(%q): want rejection, got success", tc.resource)
			}
			// A rejected AcquireLock must not have left a row behind.
			got, err := s.GetLock(tc.resource)
			if err != nil {
				t.Fatalf("GetLock(%q): %v", tc.resource, err)
			}
			if got != nil {
				t.Fatalf("GetLock(%q): rejected acquire left a row behind: %+v", tc.resource, got)
			}
		})
	}
}

// TestResourceNameExactMatchNoNormalization proves two names differing
// only by case, or only by trailing whitespace, are genuinely distinct
// resources -- both independently lockable at the same time -- not
// silently folded together by some normalization pass this project never
// claims to have. locks.resource is a plain TEXT PRIMARY KEY with SQLite's
// default BINARY collation, so this is really testing "nothing upstream of
// the SQL layer surprises us," not the SQL layer itself.
func TestResourceNameExactMatchNoNormalization(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	pairs := [][2]string{
		{"Resource", "resource"},
		{"RESOURCE", "resource"},
		{"resource", "resource "},  // trailing space
		{"resource", " resource"},  // leading space
	}
	for _, p := range pairs {
		t.Run(p[0]+"_vs_"+p[1], func(t *testing.T) {
			s := newTestStore(t) // fresh store per pair, avoids cross-pair collisions
			if _, err := s.AcquireLock(p[0], alpha.ID, "", "", 0); err != nil {
				t.Fatalf("AcquireLock(%q): %v", p[0], err)
			}
			// If these collapsed to the same row, this would spuriously
			// succeed as a same-holder re-acquire OR fail as ErrLockHeld
			// against a *different* apparent holder -- it must instead
			// succeed as a brand new, independent lock.
			if _, err := s.AcquireLock(p[1], alpha.ID, "", "", 0); err != nil {
				t.Fatalf("AcquireLock(%q) as a distinct resource from %q: %v", p[1], p[0], err)
			}
			all, err := s.ListLocks()
			if err != nil {
				t.Fatalf("ListLocks: %v", err)
			}
			if len(all) != 2 {
				t.Fatalf("want 2 distinct locks for %q and %q, got %d: %+v", p[0], p[1], len(all), all)
			}
		})
	}
}

// TestResourceNameSQLInjectionShapedStringsAreSafe is the explicit proof
// (not an assumption from "this project uses parameterized queries
// elsewhere") that a resource name shaped like a SQL injection attempt
// never touches the database as anything other than inert string data:
// the store keeps working normally afterward, and a real different
// resource is unaffected.
func TestResourceNameSQLInjectionShapedStringsAreSafe(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	evil := `res'; DROP TABLE locks; DROP TABLE lock_events; --`
	if _, err := s.AcquireLock(evil, alpha.ID, "note", "", 0); err != nil {
		t.Fatalf("AcquireLock(%q): %v", evil, err)
	}
	if _, err := s.CreateInterest(alpha.ID, evil, "watch"); err != nil {
		t.Fatalf("CreateInterest(%q): %v", evil, err)
	}

	// If the string had ever been concatenated into SQL instead of bound
	// as a parameter, the locks/lock_events tables would be gone by now.
	if _, err := s.AcquireLock("innocent-bystander", alpha.ID, "note", "", 0); err != nil {
		t.Fatalf("AcquireLock(innocent-bystander) after SQL-shaped resource name: %v -- tables likely damaged", err)
	}
	got, err := s.GetLock(evil)
	if err != nil {
		t.Fatalf("GetLock(%q) after SQL-shaped resource name: %v -- tables likely damaged", evil, err)
	}
	if got == nil || got.Resource != evil {
		t.Fatalf("GetLock(%q) = %+v, want the exact row still intact", evil, got)
	}
	all, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks after SQL-shaped resource name: %v -- tables likely damaged", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 locks (evil + innocent-bystander), got %d: %+v", len(all), all)
	}
}

// ── 2. Lease field-type edge cases ──────────────────────────────────────────

// TestAcquireLockRejectsNegativeLeaseDirectly is the check this project's
// own history says is worth doing: cmdLockAcquire's CLI flag parser
// already rejects -lease<0 before it ever reaches AcquireLock, but nothing
// stops a caller going through Store's Go API directly (as this test does)
// from passing lease_seconds:-1 and bypassing the CLI's validation
// entirely. This proves what AcquireLock itself actually does with a
// negative value when nothing upstream has
// screened it out.
func TestAcquireLockRejectsNegativeLeaseDirectly(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "note", "", -5); err == nil {
		t.Fatal("AcquireLock with a negative leaseSeconds should be rejected, not silently accepted")
	}
	// A rejected acquire must not have created a row (mirrors the same
	// invariant TestAcquireLockRejectsUnknownStrategy already checks for
	// the strategy-id validation path).
	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got != nil {
		t.Fatalf("a rejected negative-lease acquire should not have created a lock row, got %+v", got)
	}
}

// TestAcquireLockLeaseZeroMeansNoLease pins down leaseColumns' own stated
// contract (leaseSeconds<=0 means "no lease," must land as SQL NULL, not a
// stored 0) against the exact value every pre-existing call site actually
// passes.
func TestAcquireLockLeaseZeroMeansNoLease(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	l, err := s.AcquireLock("res", alpha.ID, "note", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock(lease=0): %v", err)
	}
	if l.LeaseSeconds != 0 || l.RenewedAt != nil {
		t.Fatalf("lease=0 should mean no lease at all, got LeaseSeconds=%d RenewedAt=%v", l.LeaseSeconds, l.RenewedAt)
	}
	if l.Expired(time.Now().Add(999 * time.Hour)) {
		t.Fatalf("a lease=0 lock must never report Expired, at any distance")
	}
}

// TestLeaseVeryLargeValueDoesNotOverflowExpiryArithmetic is the direct,
// run-it-for-real check for the question this project's own culture insists
// on answering by observation, not by reasoning: does a multi-year lease
// overflow anything in the timestamp arithmetic? A realistic multi-year
// lease (100 years) must stay correctly "live" right after being acquired.
func TestLeaseVeryLargeValueDoesNotOverflowExpiryArithmetic(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	const hundredYearsSeconds = 100 * 365 * 24 * 3600 // ~3.1536e9, realistic "multi-year lease"
	l, err := s.AcquireLock("res", alpha.ID, "note", "", hundredYearsSeconds)
	if err != nil {
		t.Fatalf("AcquireLock(100-year lease): %v", err)
	}
	if l.Expired(time.Now()) {
		t.Fatalf("a freshly acquired 100-year lease must not read as expired")
	}
	// Even after a very real amount of backdating, still nowhere near expiry.
	backdateRenewedAt(t, s, "res", 24*time.Hour)
	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.Expired(time.Now()) {
		t.Fatalf("a 100-year lease backdated by only 24h must still read as live")
	}
}

// TestLeaseExtremeValueOverflowsDurationArithmetic is the adversarial
// sibling of the 100-year test above: Lock.Expired computes
// time.Duration(l.LeaseSeconds)*time.Second, and time.Duration is a signed
// int64 count of *nanoseconds* -- its range tops out around 292 years. A
// leaseSeconds value comfortably representable as a plain Go int (and
// therefore something any caller of Store's Go API can pass) but past
// that ~292-year nanosecond ceiling silently overflows the
// multiplication. This test proves, by actually running it, whether that
// overflow makes a lease that was *just* acquired read back as already
// expired -- the opposite of what the caller asked for, and a real
// liveness gap: anyone racing to reclaim would win a resource whose
// "owner" never actually let go.
func TestLeaseExtremeValueOverflowsDurationArithmetic(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	// ~294.7 years in seconds -- past time.Duration's ~292-year ceiling
	// once multiplied out to nanoseconds, but an entirely ordinary int.
	const extremeLeaseSeconds = 9_300_000_000
	l, err := s.AcquireLock("res", alpha.ID, "note", "", extremeLeaseSeconds)
	if err != nil {
		t.Fatalf("AcquireLock(extreme lease): %v", err)
	}
	if l.Expired(time.Now()) {
		t.Fatalf("BUG CONFIRMED: a lease acquired *this instant* with leaseSeconds=%d already reads as Expired -- integer overflow in the duration arithmetic", extremeLeaseSeconds)
	}
	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if got.Expired(time.Now()) {
		t.Fatalf("BUG CONFIRMED: GetLock's freshly-read lease with leaseSeconds=%d already reads as Expired -- integer overflow in the duration arithmetic", extremeLeaseSeconds)
	}
}

// TestLeaseExpiryBoundaryConsistentAcrossReadPaths proves the ">= " boundary
// decision in Lock.Expired agrees with the identical ">=" condition
// reclaimExpiredLock's SQL WHERE clause uses at write time -- GetLock,
// ListLocks, and the actual reclaim path all have to treat
// "renewed_at + lease_seconds == now" the same way, not accidentally
// differ between the Go-side check and the SQL-side one.
func TestLeaseExpiryBoundaryConsistentAcrossReadPaths(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "note", "", 5); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	// Backdate to exactly the boundary instant (renewed_at + 5s == now);
	// by the time the assertions below run, real wall-clock time has moved
	// at least a little past that instant, which is exactly the ">="
	// condition under test.
	backdateRenewedAt(t, s, "res", 5*time.Second)

	viaGetLock, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if !viaGetLock.Expired(time.Now()) {
		t.Fatalf("GetLock: lock at/past the exact expiry boundary must read as Expired")
	}

	viaListLocks, err := s.ListLocks()
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(viaListLocks) != 1 || !viaListLocks[0].Expired(time.Now()) {
		t.Fatalf("ListLocks: lock at/past the exact expiry boundary must read as Expired, got %+v", viaListLocks)
	}

	// The sharpest proof: the SQL-side reclaim path must agree too --
	// beta must actually be able to reclaim right at this boundary,
	// without -force.
	claimed, err := s.AcquireLock("res", beta.ID, "beta reclaims at the boundary", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock(beta) at the exact expiry boundary: want a successful reclaim, got error: %v", err)
	}
	if !claimed.Reclaimed || claimed.HolderID != beta.ID {
		t.Fatalf("expected a reclaim by beta at the boundary, got %+v", claimed)
	}
}

// ── 3. Real concurrency/timing scenarios ────────────────────────────────────

// TestConcurrentReclaimOfExpiredLeaseHasExactlyOneWinner is the reclaim
// path's sibling to e2e_test.go's TestConcurrentAcquireHasExactlyOneWinner
// -- the sharpest possible test of reclaimExpiredLock's conditioned UPDATE:
// real OS processes, not goroutines, racing to reclaim the exact same
// lease-expired resource at the same instant. Exactly one may win the
// reclaim; everyone else must lose cleanly, never with a raw SQL error and
// never with more than one actually taking the resource over.
func TestConcurrentReclaimOfExpiredLeaseHasExactlyOneWinner(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "reclaim-race.db")

	// Setup: a real leased acquire, then backdate it stale, all through a
	// direct Store handle so the exact same on-disk DB is ready before any
	// racer process starts. Closed before the race begins so it can't hold
	// any lingering connection open against the racers.
	setup, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := setup.AcquireLock("race", "orig-holder", "orig's step", "", 3); err != nil {
		setup.Close()
		t.Fatalf("setup AcquireLock: %v", err)
	}
	backdateRenewedAt(t, setup, "race", time.Hour) // 3s lease, an hour stale
	setup.Close()

	const n = 12
	codes := make([]int, n)
	outs := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := exec.Command(bin, "lock", "acquire", "-db="+db, "-resource=race", "-identity=reclaimer-"+strconv.Itoa(i))
			out, err := cmd.CombinedOutput()
			outs[i] = string(out)
			switch e := err.(type) {
			case nil:
				codes[i] = 0
			case *exec.ExitError:
				codes[i] = e.ExitCode()
			default:
				t.Errorf("reclaimer %d: unexpected error running binary: %v", i, err)
				codes[i] = -1
			}
		}(i)
	}
	close(start)
	wg.Wait()

	wins, losses := 0, 0
	for i, code := range codes {
		switch code {
		case 0:
			wins++
			if !strings.Contains(outs[i], "reclaimed") {
				t.Errorf("reclaimer %d: won but output doesn't say it was a reclaim:\n%s", i, outs[i])
			}
		case 1:
			losses++
			if !strings.Contains(outs[i], "resource is locked by another identity") {
				t.Errorf("reclaimer %d: lost, but not with the clean denial message — got:\n%s", i, outs[i])
			}
		default:
			t.Errorf("reclaimer %d: unexpected exit code %d, output:\n%s", i, code, outs[i])
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 winner among %d concurrent reclaims, got %d wins, %d losses (codes=%v)", n, wins, losses, codes)
	}
	if losses != n-1 {
		t.Fatalf("want %d clean losses, got %d", n-1, losses)
	}

	// Exactly one "reclaimed" event, ever, on the audit trail.
	final, err := openStore(db)
	if err != nil {
		t.Fatalf("re-opening db to inspect events: %v", err)
	}
	defer final.Close()
	rows, err := final.db.Query(`SELECT COUNT(*) FROM lock_events WHERE resource = 'race' AND kind = 'reclaimed'`)
	if err != nil {
		t.Fatalf("query lock_events: %v", err)
	}
	defer rows.Close()
	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 'reclaimed' lock_event, got %d", count)
	}
}

// TestRenewVsReclaimAtRealExpiryBoundary is a real-wall-clock companion to
// the backdating-based boundary test above: a 1-second real lease, and at
// roughly the same instant it expires, one goroutine tries to renew while
// several others try to reclaim it. At any given instant the resource is
// either still live (renew wins, every reclaimer is correctly denied) or
// already expired (renew is correctly refused, exactly one reclaimer
// wins) -- across the whole batch, exactly one call may ever succeed.
func TestRenewVsReclaimAtRealExpiryBoundary(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "alpha's step", "", 1); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	const nReclaimers = 8
	var successes int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := s.RenewLock("res", alpha.ID); err == nil {
			atomic.AddInt32(&successes, 1)
		}
	}()
	for i := 0; i < nReclaimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := s.CreateIdentity("reclaimer-" + strconv.Itoa(i))
			if err != nil {
				t.Errorf("CreateIdentity: %v", err)
				return
			}
			<-start
			claimed, err := s.AcquireLock("res", id.ID, "reclaiming", "", 0)
			if err == nil && claimed.Reclaimed {
				atomic.AddInt32(&successes, 1)
			}
		}(i)
	}

	// Real wall-clock time: land the whole burst right around the 1s
	// expiry instant, not exactly at it -- some runs will catch it live,
	// some expired, both are valid and both must still yield exactly one
	// winner.
	time.Sleep(950 * time.Millisecond)
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("want exactly 1 successful outcome across 1 renew + %d reclaim attempts racing the real expiry boundary, got %d", nReclaimers, successes)
	}
}

// TestRapidAcquireReleaseReacquireCycles is idempotency under real repeated
// pressure, not just the existing twice-in-a-row test: the same identity
// acquiring, releasing, and re-acquiring the same resource 50 times in a
// row, plus interleaved same-holder re-acquires (no release in between)
// every few cycles, all against a real SQLite file, all expected to
// succeed cleanly every single time.
func TestRapidAcquireReleaseReacquireCycles(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	const cycles = 50
	for i := 0; i < cycles; i++ {
		note := fmt.Sprintf("cycle %d", i)
		if _, err := s.AcquireLock("res", alpha.ID, note, "", 0); err != nil {
			t.Fatalf("cycle %d: AcquireLock: %v", i, err)
		}
		if i%5 == 0 {
			// Same-holder re-acquire without releasing first -- must stay
			// idempotent under repetition, not just the first time.
			if _, err := s.AcquireLock("res", alpha.ID, note+"-reacquire", "", 0); err != nil {
				t.Fatalf("cycle %d: same-holder re-acquire: %v", i, err)
			}
		}
		if err := s.ReleaseLock("res", alpha.ID, note+"-release", false); err != nil {
			t.Fatalf("cycle %d: ReleaseLock: %v", i, err)
		}
		if got, err := s.GetLock("res"); err != nil {
			t.Fatalf("cycle %d: GetLock: %v", i, err)
		} else if got != nil {
			t.Fatalf("cycle %d: resource should be free after release, got holder %q", i, got.HolderID)
		}
	}
}

// TestShortLeaseExpiresUnderRealWallClock is the real-time counterpart to
// every other lease-expiry test in this suite, which all use the
// backdating technique: an actual 1-second lease, an actual time.Sleep
// past it, no direct SQL manipulation of renewed_at at all -- proving the
// arithmetic holds up against the real clock a live agent process would
// actually see, not just a synthetically rewritten timestamp.
func TestShortLeaseExpiresUnderRealWallClock(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	l, err := s.AcquireLock("res", alpha.ID, "alpha's step", "", 1)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.Expired(time.Now()) {
		t.Fatalf("a freshly acquired 1s lease must not read as expired immediately")
	}

	time.Sleep(1200 * time.Millisecond) // real wall-clock time past the 1s lease

	got, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if !got.Expired(time.Now()) {
		t.Fatalf("a 1s lease must read as expired after a real 1.2s sleep")
	}

	claimed, err := s.AcquireLock("res", beta.ID, "beta takes over for real", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock(beta) after real expiry: %v", err)
	}
	if !claimed.Reclaimed || claimed.HolderID != beta.ID {
		t.Fatalf("beta should have reclaimed the resource after real wall-clock expiry, got %+v", claimed)
	}
}

// ── 4. Strategy-linkage field-type edge cases ───────────────────────────────

// TestAcquireLockWithSQLMeaningfulStrategyID proves a strategy id shaped
// like a SQL injection attempt is handled the same safe way a
// nonexistent-but-ordinary strategy id already is (per
// TestAcquireLockRejectsUnknownStrategy): rejected cleanly as "not found,"
// never touching the strategies table as anything but inert parameter
// data.
func TestAcquireLockWithSQLMeaningfulStrategyID(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	evilStrategyID := `strat'; DROP TABLE strategies; --`

	if _, err := s.AcquireLock("res", alpha.ID, "note", evilStrategyID, 0); err == nil {
		t.Fatal("AcquireLock with a SQL-shaped nonexistent strategy id should be rejected")
	}
	l, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock: %v -- strategies table likely damaged", err)
	}
	if l != nil {
		t.Fatalf("a rejected acquire should not have created a lock row, got %+v", l)
	}

	// Prove the strategies table is still fully intact and usable.
	st, err := s.CreateStrategy("still-works", "table survived the SQL-shaped id")
	if err != nil {
		t.Fatalf("CreateStrategy after SQL-shaped strategy id: %v -- table likely damaged", err)
	}
	if _, err := s.AcquireLock("res2", alpha.ID, "note", st.ID, 0); err != nil {
		t.Fatalf("AcquireLock with a real strategy id after the SQL-shaped attempt: %v", err)
	}
}

// TestManyLocksLinkedToStrategyNextSplitAtScale is `strategy next`'s
// linked/other split, exercised at a scale (25 linked, 15 not) the
// existing 2-3-lock coverage never reaches -- proving the split logic
// stays exactly right and doesn't, say, start truncating, double-counting,
// or misclassifying once there are real numbers of locks involved.
func TestManyLocksLinkedToStrategyNextSplitAtScale(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "strategy-next-scale.db")

	// -db has to land before any positional argument (here, "strategy
	// next <id>") -- Go's flag package stops parsing at the first
	// non-flag token, so a trailing -db would never actually get parsed.
	run := func(args ...string) (int, string) {
		if len(args) < 2 {
			t.Fatalf("run: need at least <cmd> <subcommand>, got %v", args)
		}
		full := append([]string{args[0], args[1], "-db=" + db}, args[2:]...)
		out, err := exec.Command(bin, full...).CombinedOutput()
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

	idCode, idOut := run("identity", "create", "-label=agent")
	identity := extractID(t, idCode, idOut)

	targetCode, targetOut := run("strategy", "create", "-name=target", "-thesis=the one strategy next is asked about")
	target := extractID(t, targetCode, targetOut)
	otherCode, otherOut := run("strategy", "create", "-name=other", "-thesis=a different strategy")
	other := extractID(t, otherCode, otherOut)

	// Deliberately distinct prefixes with no substring overlap between
	// groups (e.g. "target-res-2" would otherwise be a substring of a
	// hypothetical "other-target-res-2"), since the membership check below
	// greps the raw CLI output for each resource's exact name.
	const linkedCount = 25
	for i := 0; i < linkedCount; i++ {
		resource := "target-res-" + strconv.Itoa(i)
		if code, out := run("lock", "acquire", "-resource="+resource, "-identity="+identity, "-strategy="+target); code != 0 {
			t.Fatalf("acquire linked lock %d failed: %s", i, out)
		}
	}

	const otherLinkedCount = 10 // linked to a *different* strategy
	for i := 0; i < otherLinkedCount; i++ {
		resource := "altstrat-res-" + strconv.Itoa(i)
		if code, out := run("lock", "acquire", "-resource="+resource, "-identity="+identity, "-strategy="+other); code != 0 {
			t.Fatalf("acquire other-strategy lock %d failed: %s", i, out)
		}
	}
	const unlinkedCount = 5
	for i := 0; i < unlinkedCount; i++ {
		resource := "plain-res-" + strconv.Itoa(i)
		if code, out := run("lock", "acquire", "-resource="+resource, "-identity="+identity); code != 0 {
			t.Fatalf("acquire unlinked lock %d failed: %s", i, out)
		}
	}
	const wantOther = otherLinkedCount + unlinkedCount

	code, out := run("strategy", "next", target)
	if code != 0 {
		t.Fatalf("strategy next failed: %s", out)
	}

	wantLinkedHeader := fmt.Sprintf("locks linked to this strategy (%d):", linkedCount)
	if !strings.Contains(out, wantLinkedHeader) {
		t.Fatalf("strategy next output missing %q, got:\n%s", wantLinkedHeader, out)
	}
	wantOtherHeader := fmt.Sprintf("other locks held system-wide (%d, not linked to this strategy):", wantOther)
	if !strings.Contains(out, wantOtherHeader) {
		t.Fatalf("strategy next output missing %q, got:\n%s", wantOtherHeader, out)
	}

	// Split by section and cross-check membership, not just the counts in
	// the header lines.
	linkedSection, otherSection, found := strings.Cut(out, "other locks held system-wide")
	if !found {
		t.Fatalf("could not split output into linked/other sections:\n%s", out)
	}
	for i := 0; i < linkedCount; i++ {
		resource := "target-res-" + strconv.Itoa(i)
		if !strings.Contains(linkedSection, resource) {
			t.Errorf("linked resource %q missing from the linked section", resource)
		}
		if strings.Contains(otherSection, resource) {
			t.Errorf("linked resource %q leaked into the other section", resource)
		}
	}
	for i := 0; i < otherLinkedCount; i++ {
		resource := "altstrat-res-" + strconv.Itoa(i)
		if !strings.Contains(otherSection, resource) {
			t.Errorf("other-strategy resource %q missing from the other section", resource)
		}
		if strings.Contains(linkedSection, resource) {
			t.Errorf("other-strategy resource %q leaked into the linked section", resource)
		}
	}
	for i := 0; i < unlinkedCount; i++ {
		resource := "plain-res-" + strconv.Itoa(i)
		if !strings.Contains(otherSection, resource) {
			t.Errorf("unlinked resource %q missing from the other section", resource)
		}
	}
}

// ── 5. Force-release and reclaim interaction ────────────────────────────────

// TestForceReleaseClearsActiveLeaseState is the direct check for a
// plausible gap flagged up front: force-release predates leases in this
// codebase's history, so it's worth confirming directly, not assuming,
// that force-releasing a lock with a still-*live* (not expired) lease
// leaves no stale lease_seconds/renewed_at behind to confuse the next
// acquire. ReleaseLock's DELETE removes the whole row rather than
// UPDATE-ing it, so by construction there's nothing to leave behind --
// this proves that's actually true end to end, not just true by reading
// the DELETE statement.
func TestForceReleaseClearsActiveLeaseState(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity(alpha): %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity(beta): %v", err)
	}

	l, err := s.AcquireLock("res", alpha.ID, "alpha's step", "", 300) // long, definitely-live lease
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if l.Expired(time.Now()) {
		t.Fatalf("setup: lease should be live, not expired")
	}

	if err := s.ReleaseLock("res", beta.ID, "force taking over", true); err != nil {
		t.Fatalf("force ReleaseLock on an active lease: %v", err)
	}

	// The row must be fully gone, not left behind with a stale lease.
	afterForce, err := s.GetLock("res")
	if err != nil {
		t.Fatalf("GetLock after force release: %v", err)
	}
	if afterForce != nil {
		t.Fatalf("resource should be completely free after force release, got %+v", afterForce)
	}

	// A fresh no-lease acquire afterward must show clean, empty lease
	// fields -- no bleed-through from the lease that was active moments
	// before the force release.
	fresh, err := s.AcquireLock("res", beta.ID, "beta's fresh hold", "", 0)
	if err != nil {
		t.Fatalf("AcquireLock after force release: %v", err)
	}
	if fresh.LeaseSeconds != 0 || fresh.RenewedAt != nil {
		t.Fatalf("fresh acquire after force-releasing an active lease should have clean lease fields, got LeaseSeconds=%d RenewedAt=%v", fresh.LeaseSeconds, fresh.RenewedAt)
	}
	if fresh.Expired(time.Now().Add(999 * time.Hour)) {
		t.Fatalf("fresh no-lease acquire must never report Expired")
	}
}
