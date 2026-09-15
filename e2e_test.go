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
