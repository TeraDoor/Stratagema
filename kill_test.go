package main

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSIGKILLMidWriteLeavesDatabaseConsistent is the scenario a durability
// pass flagged but couldn't test (its sandbox's background-command model
// didn't allow true mid-write interleaving). A real subprocess is put
// into a tight acquire/release loop against many different resources —
// so at any given instant it's likely to be actively writing — and killed
// with SIGKILL (not a clean exit) at a random-ish point. The property
// under test isn't "no work is lost" (killing a process obviously loses
// its in-flight work) — it's that the database itself is never left
// unreadable or inconsistent for the next process that opens it: SQLite's
// WAL mode is supposed to guarantee this by construction (an unfinished
// write is simply not there after a crash, not half-there), and this
// checks that guarantee actually holds for Stratagema's own schema and
// access pattern, not just in the abstract.
func TestSIGKILLMidWriteLeavesDatabaseConsistent(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "kill.db")

	// A small shell loop, not a single stratagema invocation: each
	// acquire+release is its own process/transaction, so killing the
	// *shell* mid-loop reliably lands the kill between or during one of
	// many small writes, without needing precise timing control over a
	// single long-lived process's internal state.
	script := `
i=0
while true; do
  i=$((i+1))
  "$1" lock acquire -db="$2" -resource="res-$i" -identity=killed-worker >/dev/null 2>&1
  "$1" lock release -db="$2" -resource="res-$i" -identity=killed-worker >/dev/null 2>&1
done
`
	cmd := exec.Command("sh", "-c", script, "sh", bin, db)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the write-loop subprocess: %v", err)
	}

	time.Sleep(300 * time.Millisecond) // let it get a real burst of writes going
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the write-loop subprocess: %v", err)
	}
	cmd.Wait() // reap it; a killed process's Wait error is expected and irrelevant here

	// The real check: can a fresh process open this exact database file
	// and use it normally afterward, with no corruption from the kill?
	out, err := exec.Command(bin, "lock", "list", "-db="+db).CombinedOutput()
	if err != nil {
		t.Fatalf("lock list after SIGKILL mid-write: %v\n%s", err, out)
	}

	out, err = exec.Command(bin, "lock", "acquire", "-db="+db, "-resource=post-crash-check", "-identity=fresh-worker").CombinedOutput()
	if err != nil {
		t.Fatalf("a fresh acquire after SIGKILL mid-write should succeed cleanly, got: %v\n%s", err, out)
	}
	out, err = exec.Command(bin, "lock", "release", "-db="+db, "-resource=post-crash-check", "-identity=fresh-worker").CombinedOutput()
	if err != nil {
		t.Fatalf("a fresh release after SIGKILL mid-write should succeed cleanly, got: %v\n%s", err, out)
	}
}
