package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStrategyLogTwiceThenShowPrintsInOrder is the CLI-level proof for
// the strategy ledger, exercised the same way e2e_test.go proves the
// lock property: the real compiled binary, not the Store's Go API
// directly. It proves `strategy log` run twice, then `strategy show`,
// prints both events back in the order they were actually logged (not
// reversed, not by wall-clock ts, which a fast clock on the second call
// could otherwise scramble), and that `strategy close` leaves behind a
// real final reflection event rather than just flipping status.
func TestStrategyLogTwiceThenShowPrintsInOrder(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "strategy.db")

	// run separates flags from positional args and always emits flags
	// (including -db) before any positional argument, regardless of the
	// order the caller listed them in — required because Go's flag
	// package stops parsing at the first non-flag token, so a positional
	// strategy id placed before -identity/-kind/-note (or before -db)
	// would silently prevent those flags from ever being parsed.
	run := func(cmd, sub string, rest ...string) (int, string) {
		var flags, positional []string
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				positional = append(positional, a)
			}
		}
		full := append([]string{cmd, sub, "-db=" + db}, flags...)
		full = append(full, positional...)
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

	idCode, idOut := run("identity", "create", "-label=faculty-1")
	identity := extractID(t, idCode, idOut)

	createCode, createOut := run("strategy", "create", "-name=probe", "-thesis=does the CLI log events in order")
	stratID := extractID(t, createCode, createOut)

	firstNote := "first finding: resource contention only under concurrent writers"
	secondNote := "second finding: rowid ordering avoids the clock-skew bug"
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=finding", "-note="+firstNote, stratID); code != 0 {
		t.Fatalf("strategy log (1st) failed (exit %d): %s", code, out)
	}
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=decision", "-note="+secondNote, stratID); code != 0 {
		t.Fatalf("strategy log (2nd) failed (exit %d): %s", code, out)
	}

	showCode, showOut := run("strategy", "show", stratID)
	if showCode != 0 {
		t.Fatalf("strategy show failed (exit %d): %s", showCode, showOut)
	}
	firstIdx := strings.Index(showOut, firstNote)
	secondIdx := strings.Index(showOut, secondNote)
	if firstIdx == -1 || secondIdx == -1 {
		t.Fatalf("strategy show didn't print both events, got:\n%s", showOut)
	}
	if firstIdx > secondIdx {
		t.Fatalf("strategy show printed the 2nd logged event before the 1st, got:\n%s", showOut)
	}

	// Reject an unknown -kind cleanly, rather than silently accepting it.
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=made-up", "-note=x", stratID); code == 0 {
		t.Fatalf("strategy log with an unknown -kind should fail, got exit 0: %s", out)
	}

	outcome := "closing: both findings confirmed the ledger orders correctly"
	closeCode, closeOut := run("strategy", "close", "-identity="+identity, "-outcome="+outcome, stratID)
	if closeCode != 0 {
		t.Fatalf("strategy close failed (exit %d): %s", closeCode, closeOut)
	}

	showCode, showOut = run("strategy", "show", stratID)
	if showCode != 0 {
		t.Fatalf("strategy show (after close) failed (exit %d): %s", showCode, showOut)
	}
	if !strings.Contains(showOut, "status:  closed") {
		t.Fatalf("strategy show should report status closed after close, got:\n%s", showOut)
	}
	if !strings.Contains(showOut, outcome) {
		t.Fatalf("strategy show should include the close outcome as a logged reflection event, got:\n%s", showOut)
	}
	if !strings.Contains(showOut, "reflection") {
		t.Fatalf("closing should leave a reflection-kind event behind, got:\n%s", showOut)
	}
	// closing must come after both findings, not replace them.
	if !strings.Contains(showOut, firstNote) || !strings.Contains(showOut, secondNote) {
		t.Fatalf("strategy show after close should still contain the earlier logged events, got:\n%s", showOut)
	}
}

// TestStrategyNextRecapsRecentEventsAndSystemWideLocks is the CLI-level
// proof for `strategy next`: run through the real compiled binary, it
// must (1) print the strategy's current status and thesis, (2) recap
// only the events since the last real step_completed — an older finding
// logged before that boundary must NOT appear, while everything from the
// boundary onward must — and (3) list a lock acquired by a totally
// unrelated identity against the same database as a system-wide
// courtesy, unlinked to this strategy. It also asserts the output never
// contains anything that looks like the command deciding something on
// the agent's behalf (e.g. a "recommend" field), since that boundary is
// the one hard constraint on this command.
func TestStrategyNextRecapsRecentEventsAndSystemWideLocks(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "strategy-next.db")

	run := func(cmd, sub string, rest ...string) (int, string) {
		var flags, positional []string
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				positional = append(positional, a)
			}
		}
		full := append([]string{cmd, sub, "-db=" + db}, flags...)
		full = append(full, positional...)
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

	idCode, idOut := run("identity", "create", "-label=faculty-resuming-work")
	identity := extractID(t, idCode, idOut)

	createCode, createOut := run("strategy", "create", "-name=recap-probe", "-thesis=resuming agents get an accurate, non-deciding recap")
	stratID := extractID(t, createCode, createOut)

	// Pre-boundary history: a step that actually finished. This note
	// must NOT show up in `strategy next`'s recap — it's exactly the
	// kind of already-settled context a resuming agent shouldn't need to
	// re-read.
	staleFinding := "stale finding: this belongs to the step that already completed"
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=finding", "-note="+staleFinding, stratID); code != 0 {
		t.Fatalf("strategy log (stale finding) failed (exit %d): %s", code, out)
	}
	stepDoneNote := "step 1 done: initial investigation complete"
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=step_completed", "-note="+stepDoneNote, stratID); code != 0 {
		t.Fatalf("strategy log (step_completed) failed (exit %d): %s", code, out)
	}

	// Post-boundary history: this is what `strategy next` should recap.
	freshFinding := "fresh finding: contention only shows up under real concurrency"
	freshDecision := "fresh decision: adopt the rowid-ordered cutoff, not wall-clock ts"
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=finding", "-note="+freshFinding, stratID); code != 0 {
		t.Fatalf("strategy log (fresh finding) failed (exit %d): %s", code, out)
	}
	if code, out := run("strategy", "log", "-identity="+identity, "-kind=decision", "-note="+freshDecision, stratID); code != 0 {
		t.Fatalf("strategy log (fresh decision) failed (exit %d): %s", code, out)
	}
	if code, out := run("strategy", "activate", stratID); code != 0 {
		t.Fatalf("strategy activate failed (exit %d): %s", code, out)
	}

	// A real lock, held by a completely unrelated identity, against the
	// same database — strategies and locks aren't formally linked, so
	// `strategy next` should still surface it as system-wide context.
	otherIdCode, otherIdOut := run("identity", "create", "-label=unrelated-agent")
	otherIdentity := extractID(t, otherIdCode, otherIdOut)
	if code, out := run("lock", "acquire", "-resource=some/other/file.go", "-identity="+otherIdentity, "-note=unrelated edit"); code != 0 {
		t.Fatalf("lock acquire failed (exit %d): %s", code, out)
	}

	nextCode, nextOut := run("strategy", "next", stratID)
	if nextCode != 0 {
		t.Fatalf("strategy next failed (exit %d): %s", nextCode, nextOut)
	}

	if !strings.Contains(nextOut, "status:  active") {
		t.Fatalf("strategy next should report the current status, got:\n%s", nextOut)
	}
	if !strings.Contains(nextOut, "resuming agents get an accurate, non-deciding recap") {
		t.Fatalf("strategy next should include the thesis, got:\n%s", nextOut)
	}

	if strings.Contains(nextOut, staleFinding) {
		t.Fatalf("strategy next should NOT include the pre-step_completed finding, got:\n%s", nextOut)
	}
	if !strings.Contains(nextOut, stepDoneNote) {
		t.Fatalf("strategy next should include the step_completed boundary event itself, got:\n%s", nextOut)
	}
	if !strings.Contains(nextOut, freshFinding) {
		t.Fatalf("strategy next should include the post-boundary finding, got:\n%s", nextOut)
	}
	if !strings.Contains(nextOut, freshDecision) {
		t.Fatalf("strategy next should include the post-boundary decision, got:\n%s", nextOut)
	}

	if !strings.Contains(nextOut, "some/other/file.go") || !strings.Contains(nextOut, otherIdentity) {
		t.Fatalf("strategy next should list the system-wide held lock and its holder, got:\n%s", nextOut)
	}

	// The hard constraint: this command must never decide anything.
	lower := strings.ToLower(nextOut)
	for _, forbidden := range []string{"recommend", "should do", "next step:", "suggested action"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("strategy next must never make a recommendation (found %q), got:\n%s", forbidden, nextOut)
		}
	}
}

// TestStrategyNextDistinguishesLinkedLocks is the CLI-level proof that
// `strategy next` now reads a real link, not the old "every held lock,
// informational only" dump: a lock acquired with -strategy=<this strategy>
// must appear in the linked section, and a lock acquired with no -strategy
// at all must appear only in the "other" section, unlinked.
func TestStrategyNextDistinguishesLinkedLocks(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "strategy-linked-locks.db")

	run := func(cmd, sub string, rest ...string) (int, string) {
		var flags, positional []string
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				positional = append(positional, a)
			}
		}
		full := append([]string{cmd, sub, "-db=" + db}, flags...)
		full = append(full, positional...)
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

	idCode, idOut := run("identity", "create", "-label=agent-doing-the-work")
	identity := extractID(t, idCode, idOut)

	createCode, createOut := run("strategy", "create", "-name=lock-link-probe", "-thesis=strategy next distinguishes linked locks from other locks")
	stratID := extractID(t, createCode, createOut)

	otherStratCode, otherStratOut := run("strategy", "create", "-name=unrelated-strategy", "-thesis=a different strategy entirely")
	otherStratID := extractID(t, otherStratCode, otherStratOut)

	// A lock explicitly linked to the strategy under test.
	if code, out := run("lock", "acquire", "-resource=src/pool.go", "-identity="+identity, "-note=working on it", "-strategy="+stratID); code != 0 {
		t.Fatalf("lock acquire (linked) failed (exit %d): %s", code, out)
	}
	// A lock with no -strategy at all — must stay unlinked.
	if code, out := run("lock", "acquire", "-resource=docs/readme.md", "-identity="+identity, "-note=side task"); code != 0 {
		t.Fatalf("lock acquire (unlinked) failed (exit %d): %s", code, out)
	}
	// A lock linked to a *different* strategy — must show up as "other", not as linked.
	if code, out := run("lock", "acquire", "-resource=config/db.yaml", "-identity="+identity, "-note=unrelated work", "-strategy="+otherStratID); code != 0 {
		t.Fatalf("lock acquire (other strategy) failed (exit %d): %s", code, out)
	}

	nextCode, nextOut := run("strategy", "next", stratID)
	if nextCode != 0 {
		t.Fatalf("strategy next failed (exit %d): %s", nextCode, nextOut)
	}

	linkedIdx := strings.Index(nextOut, "locks linked to this strategy")
	otherIdx := strings.Index(nextOut, "other locks held system-wide")
	if linkedIdx == -1 || otherIdx == -1 {
		t.Fatalf("strategy next should print both a linked-locks section and an other-locks section, got:\n%s", nextOut)
	}
	if linkedIdx > otherIdx {
		t.Fatalf("expected the linked-locks section before the other-locks section, got:\n%s", nextOut)
	}
	linkedSection := nextOut[linkedIdx:otherIdx]
	otherSection := nextOut[otherIdx:]

	if !strings.Contains(linkedSection, "src/pool.go") {
		t.Fatalf("linked-locks section should contain the lock explicitly acquired for this strategy, got:\n%s", linkedSection)
	}
	if strings.Contains(linkedSection, "docs/readme.md") || strings.Contains(linkedSection, "config/db.yaml") {
		t.Fatalf("linked-locks section should not contain locks that aren't linked to this strategy, got:\n%s", linkedSection)
	}

	if !strings.Contains(otherSection, "docs/readme.md") || !strings.Contains(otherSection, "config/db.yaml") {
		t.Fatalf("other-locks section should contain both the unlinked lock and the lock linked to a different strategy, got:\n%s", otherSection)
	}
	if strings.Contains(otherSection, "src/pool.go") {
		t.Fatalf("other-locks section should not contain the lock linked to this strategy, got:\n%s", otherSection)
	}

	// The now-obsolete "informational only" framing must be gone — the
	// link is real now.
	if strings.Contains(nextOut, "informational only") {
		t.Fatalf("strategy next should no longer claim locks are informational-only, got:\n%s", nextOut)
	}
}
