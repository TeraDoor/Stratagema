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
