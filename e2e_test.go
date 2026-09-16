package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildBinary compiles the real CLI once per test run — this test exercises
// the actual external binary a real agent process would invoke, not the
// Store's Go API directly, because the property under test only means
// something at that level: real OS processes racing for the same lock.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stratagema-e2e")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// TestConcurrentAcquireHasExactlyOneWinner is this project's single most
// important correctness property, tested the way it actually matters: N
// real OS processes racing for the same named lock at the same instant —
// not N sequential calls, which would never exercise the actual race.
// Exactly one may win; everyone else must fail cleanly (exit 1), never
// with an unexpected error.
func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "race.db")

	const n = 12
	codes := make([]int, n)
	stderr := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := exec.Command(bin, "lock", "acquire", "-db="+db, "-resource=race", "-identity=agent-"+string(rune('a'+i)))
			out, err := cmd.CombinedOutput()
			stderr[i] = string(out)
			switch e := err.(type) {
			case nil:
				codes[i] = 0
			case *exec.ExitError:
				codes[i] = e.ExitCode()
			default:
				t.Errorf("agent %d: unexpected error running binary: %v", i, err)
				codes[i] = -1
			}
		}(i)
	}
	close(start) // release all goroutines at once, so the acquires actually overlap
	wg.Wait()

	wins, losses := 0, 0
	for i, code := range codes {
		switch code {
		case 0:
			wins++
		case 1:
			losses++
			// A losing exit code alone isn't proof of correctness — a
			// second, uncoordinated INSERT hitting SQLite's UNIQUE
			// constraint on `locks.resource` would *also* exit 1, but
			// as a raw, unhandled SQL error, not the clean "denied"
			// path that records a lock_event and notifies subscribers.
			// This is the actual race worth catching.
			if !strings.Contains(stderr[i], "resource is locked by another identity") {
				t.Errorf("agent %d: lost, but not with the clean denial message — got:\n%s", i, stderr[i])
			}
		default:
			t.Errorf("agent %d: unexpected exit code %d, output:\n%s", i, code, stderr[i])
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 winner among %d concurrent acquires, got %d wins, %d losses (codes=%v)", n, wins, losses, codes)
	}
	if losses != n-1 {
		t.Fatalf("want %d clean losses, got %d", n-1, losses)
	}
}

// TestConcurrentForceReleaseProducesExactlyOneEvent is the regression
// test for the fix proposed and approved in the fix-force-release-race
// proto-strategy (docs/examples/stratagema-proto-strategy, in the boat
// repo): N real processes force-releasing the same lock at the same
// instant must all exit cleanly — either 0 (idempotent: it hit the
// narrow window this fix closes, and found the row already gone) or 1
// with "is not locked" (its own initial read already found nothing —
// pre-existing, correct, unrelated to this fix) — but only exactly one
// of them may actually record the "released" event and notify
// subscribers. Before the fix, every racer that got past the initial
// read did.
func TestConcurrentForceReleaseProducesExactlyOneEvent(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "release-race.db")

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

	watcherCode, watcherOut := run("identity", "create", "-label=watcher")
	watcher := extractID(t, watcherCode, watcherOut)
	if _, out := run("interest", "create", "-identity="+watcher, "-resource=race"); !strings.Contains(out, "created") {
		t.Fatalf("interest create failed: %s", out)
	}
	if code, out := run("lock", "acquire", "-resource=race", "-identity=holder"); code != 0 {
		t.Fatalf("setup acquire failed: %s", out)
	}

	const n = 10
	codes := make([]int, n)
	outs := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := exec.Command(bin, "lock", "release", "-db="+db, "-resource=race", "-identity=releaser-"+string(rune('a'+i)), "-force")
			out, err := cmd.CombinedOutput()
			outs[i] = string(out)
			switch e := err.(type) {
			case nil:
				codes[i] = 0
			case *exec.ExitError:
				codes[i] = e.ExitCode()
			default:
				t.Errorf("releaser %d: unexpected error: %v", i, err)
				codes[i] = -1
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// Two outcomes are both "clean" here, and which one a given racer
	// gets depends on timing that isn't worth controlling for: exit 0
	// (it hit the narrow window between another racer's read and delete,
	// and found the row already gone -- the idempotent path this fix
	// adds) or exit 1 with "is not locked" (its own initial read already
	// found nothing -- the pre-existing, correct behavior for
	// force-releasing something that isn't held, unrelated to this fix).
	// What's never acceptable is a raw, unhandled error.
	for i, code := range codes {
		switch code {
		case 0:
		case 1:
			if !strings.Contains(outs[i], "is not locked") {
				t.Errorf("releaser %d: exit 1 but not the clean \"not locked\" message — got:\n%s", i, outs[i])
			}
		default:
			t.Errorf("releaser %d: unexpected exit code %d:\n%s", i, code, outs[i])
		}
	}

	_, inbox := run("interest", "inbox", "-identity="+watcher)
	deliveries := strings.Count(inbox, "released")
	if deliveries != 1 {
		t.Fatalf("want exactly 1 'released' delivery from %d concurrent force-releases of the same lock, got %d:\n%s", n, deliveries, inbox)
	}
}

// extractID pulls the id off the first line of an "identity create"/
// similar "created <id>" response.
func extractID(t *testing.T, code int, out string) string {
	t.Helper()
	if code != 0 {
		t.Fatalf("command failed (exit %d): %s", code, out)
	}
	fields := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	if len(fields) < 2 {
		t.Fatalf("couldn't parse id from output: %q", out)
	}
	return fields[1]
}
