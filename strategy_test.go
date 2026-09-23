package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCreateGetListStrategy is a basic table-driven pass over the
// create/get/list surface: a freshly created strategy starts in
// "planning", GetStrategy round-trips it, and ListStrategies returns
// every strategy created so far.
func TestCreateGetListStrategy(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name   string
		thesis string
	}{
		{"tactic-file-rollout", "Tactic files reduce coordination overhead vs. ad hoc prompts"},
		{"lock-contention-probe", "Two real agents on the same file will actually contend, not just in theory"},
	}

	var created []*Strategy
	for _, tc := range cases {
		st, err := s.CreateStrategy(tc.name, tc.thesis)
		if err != nil {
			t.Fatalf("CreateStrategy(%q): %v", tc.name, err)
		}
		if st.ID == "" {
			t.Fatalf("CreateStrategy(%q): empty id", tc.name)
		}
		if st.Status != StrategyPlanning {
			t.Fatalf("CreateStrategy(%q): want status %q, got %q", tc.name, StrategyPlanning, st.Status)
		}
		if st.ClosedAt != nil {
			t.Fatalf("CreateStrategy(%q): want nil ClosedAt, got %v", tc.name, st.ClosedAt)
		}
		created = append(created, st)
	}

	for _, want := range created {
		got, err := s.GetStrategy(want.ID)
		if err != nil {
			t.Fatalf("GetStrategy(%s): %v", want.ID, err)
		}
		if got == nil {
			t.Fatalf("GetStrategy(%s): want a strategy, got nil", want.ID)
		}
		if got.Name != want.Name || got.Thesis != want.Thesis || got.Status != want.Status {
			t.Fatalf("GetStrategy(%s): got %+v, want %+v", want.ID, got, want)
		}
	}

	all, err := s.ListStrategies()
	if err != nil {
		t.Fatalf("ListStrategies: %v", err)
	}
	if len(all) != len(cases) {
		t.Fatalf("ListStrategies: want %d strategies, got %d", len(cases), len(all))
	}
}

func TestGetStrategyUnknownIDReturnsNilNil(t *testing.T) {
	s := newTestStore(t)
	st, err := s.GetStrategy("strategy-does-not-exist")
	if err != nil {
		t.Fatalf("GetStrategy: want nil error for unknown id, got %v", err)
	}
	if st != nil {
		t.Fatalf("GetStrategy: want nil strategy for unknown id, got %+v", st)
	}
}

// TestStrategyEventLogIsActuallyAppendOnly is the one test in this file
// that matters most: it proves old events never change after a new one
// is logged, not just that logging "works." A log that silently allowed
// updates would still pass a shallow "does LogStrategyEvent return no
// error" check.
func TestStrategyEventLogIsActuallyAppendOnly(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "does the log stay append-only under real writes")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	first, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_started", "began investigating resource contention")
	if err != nil {
		t.Fatalf("LogStrategyEvent(1): %v", err)
	}
	second, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "contention only appears under concurrent writers")
	if err != nil {
		t.Fatalf("LogStrategyEvent(2): %v", err)
	}
	third, err := s.LogStrategyEvent(st.ID, alpha.ID, "decision", "adopt rowid ordering, not ts")
	if err != nil {
		t.Fatalf("LogStrategyEvent(3): %v", err)
	}

	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	// Chronological order, exactly as logged.
	wantOrder := []*StrategyEvent{first, second, third}
	for i, want := range wantOrder {
		got := events[i]
		if got.ID != want.ID || got.Note != want.Note || got.Kind != want.Kind {
			t.Fatalf("event %d: got %+v, want %+v", i, got, want)
		}
	}

	// The append-only guarantee: logging a 4th event must not alter any
	// field of the first three, when read back again.
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_completed", "wrapped up the probe"); err != nil {
		t.Fatalf("LogStrategyEvent(4): %v", err)
	}
	after, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents (after 4th write): %v", err)
	}
	if len(after) != 4 {
		t.Fatalf("want 4 events after a 4th log, got %d", len(after))
	}
	for i, want := range wantOrder {
		got := after[i]
		if got.ID != want.ID || got.Note != want.Note || got.Kind != want.Kind || got.IdentityID != want.IdentityID {
			t.Fatalf("event %d changed after a later append — want %+v, got %+v", i, want, got)
		}
	}
}

func TestLogStrategyEventRejectsUnknownKind(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "made-up-kind", "note"); err == nil {
		t.Fatal("want an error for an unknown event kind")
	}
}

// TestLogStrategyEventAcceptsResourceUsage confirms resource_usage logs
// and reads back like any other event kind — schema-only support, no
// enforcement, matching the soft-reporting-first design.
func TestLogStrategyEventAcceptsResourceUsage(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	ev, err := s.LogStrategyEvent(st.ID, alpha.ID, "resource_usage", "42000 tokens, claude-code")
	if err != nil {
		t.Fatalf("LogStrategyEvent(resource_usage): %v", err)
	}
	if ev.Kind != "resource_usage" {
		t.Fatalf("Kind = %q, want %q", ev.Kind, "resource_usage")
	}

	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "resource_usage" || events[0].Note != "42000 tokens, claude-code" {
		t.Fatalf("ListStrategyEvents: got %+v, want one resource_usage event", events)
	}
}

// TestLogStrategyEventNoLongerAcceptsExternalSignal documents a
// deliberate reversal, not a regression: external_signal was added
// S077/S078, tried, and cut S081 (see validStrategyEventKinds' doc
// comment) for never gaining a real writer, unlike resource_usage. This
// pins the cut down the same way this project pins any other real
// behavior — a kind that used to be valid must now be rejected exactly
// like any other unknown kind, and the error text must not still list it.
func TestLogStrategyEventNoLongerAcceptsExternalSignal(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	_, err = s.LogStrategyEvent(st.ID, alpha.ID, "external_signal", "note")
	if err == nil {
		t.Fatal("external_signal was cut S081, want an error, got nil")
	}
	if strings.Contains(err.Error(), "|external_signal") || strings.HasSuffix(err.Error(), "external_signal)") {
		t.Fatalf("error text still lists external_signal as valid: %q", err.Error())
	}

	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("a rejected LogStrategyEvent must not have written anything, got %+v", events)
	}
}

// TestUnknownStrategyIDErrorsCleanly runs every strategy-id-taking method
// against an id that was never created and asserts each one errors
// cleanly rather than silently no-op'ing or panicking.
func TestUnknownStrategyIDErrorsCleanly(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	const badID = "strategy-does-not-exist"

	if err := s.SetStrategyStatus(badID, StrategyActive); err == nil {
		t.Error("SetStrategyStatus: want an error for an unknown strategy id")
	}
	if _, err := s.LogStrategyEvent(badID, alpha.ID, "finding", "note"); err == nil {
		t.Error("LogStrategyEvent: want an error for an unknown strategy id")
	}
	if _, err := s.ListStrategyEvents(badID); err == nil {
		t.Error("ListStrategyEvents: want an error for an unknown strategy id")
	}
	if err := s.CloseStrategy(badID, alpha.ID, "outcome"); err == nil {
		t.Error("CloseStrategy: want an error for an unknown strategy id")
	}
}

func TestSetStrategyStatusRejectsUnknownStatus(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := s.SetStrategyStatus(st.ID, "made-up-status"); err == nil {
		t.Fatal("want an error for an unknown status")
	}
}

// TestStrategyStatusTransitions exercises activate/observe (via
// SetStrategyStatus, the same function the CLI's activate/observe
// subcommands call) end to end.
func TestStrategyStatusTransitions(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if st.Status != StrategyPlanning {
		t.Fatalf("want initial status %q, got %q", StrategyPlanning, st.Status)
	}

	if err := s.SetStrategyStatus(st.ID, StrategyActive); err != nil {
		t.Fatalf("SetStrategyStatus(active): %v", err)
	}
	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Status != StrategyActive {
		t.Fatalf("want status %q after activate, got %q", StrategyActive, got.Status)
	}

	if err := s.SetStrategyStatus(st.ID, StrategyObserving); err != nil {
		t.Fatalf("SetStrategyStatus(observing): %v", err)
	}
	got, err = s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Status != StrategyObserving {
		t.Fatalf("want status %q after observe, got %q", StrategyObserving, got.Status)
	}
}

// TestRecentStrategyEventsCutsOffAtLastStepCompleted proves the actual
// cutoff logic RecentStrategyEvents implements — not just "returns some
// events," which a stub that always returned everything would also
// pass. It logs a step_completed in the middle of a longer log, then
// asserts the "recent" slice starts exactly there (including that event
// itself) and omits everything logged before it.
func TestRecentStrategyEventsCutsOffAtLastStepCompleted(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "does RecentStrategyEvents cut off at the right place")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	old1, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_started", "began step 1")
	if err != nil {
		t.Fatalf("LogStrategyEvent(old1): %v", err)
	}
	old2, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "an old finding from step 1")
	if err != nil {
		t.Fatalf("LogStrategyEvent(old2): %v", err)
	}
	boundary, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_completed", "step 1 done")
	if err != nil {
		t.Fatalf("LogStrategyEvent(boundary): %v", err)
	}
	new1, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_started", "began step 2")
	if err != nil {
		t.Fatalf("LogStrategyEvent(new1): %v", err)
	}
	new2, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "a fresh finding from step 2")
	if err != nil {
		t.Fatalf("LogStrategyEvent(new2): %v", err)
	}

	recent, err := s.RecentStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("RecentStrategyEvents: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("want 3 recent events (boundary + 2 after), got %d: %+v", len(recent), recent)
	}
	wantOrder := []*StrategyEvent{boundary, new1, new2}
	for i, want := range wantOrder {
		if recent[i].ID != want.ID {
			t.Fatalf("recent[%d]: want event %s (%s), got %s (%s)", i, want.ID, want.Note, recent[i].ID, recent[i].Note)
		}
	}
	for _, excluded := range []*StrategyEvent{old1, old2} {
		for _, got := range recent {
			if got.ID == excluded.ID {
				t.Fatalf("recent events should not include pre-boundary event %s (%s)", excluded.ID, excluded.Note)
			}
		}
	}
}

// TestRecentStrategyEventsFallsBackToWholeLogWithoutStepCompleted proves
// the other half of the cutoff: a strategy that has never logged a
// step_completed has no boundary to cut at, so every event counts as
// "recent" (subject to the cap, proved separately below).
func TestRecentStrategyEventsFallsBackToWholeLogWithoutStepCompleted(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "no step_completed logged yet")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_started", "began step 1"); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "still mid-step"); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}

	all, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	recent, err := s.RecentStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("RecentStrategyEvents: %v", err)
	}
	if len(recent) != len(all) {
		t.Fatalf("want RecentStrategyEvents to return the whole log (%d events) when no step_completed exists, got %d", len(all), len(recent))
	}
}

// TestRecentStrategyEventsCapsEvenWithoutStepCompleted proves the
// recentEventsCap bound applies to the fallback case too: a strategy
// that logs more than recentEventsCap events without ever completing a
// step must still get a short recap, not its entire history.
func TestRecentStrategyEventsCapsEvenWithoutStepCompleted(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "a long run with no step_completed")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	const total = recentEventsCap + 5
	var last *StrategyEvent
	for i := 0; i < total; i++ {
		ev, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", fmt.Sprintf("finding #%d", i))
		if err != nil {
			t.Fatalf("LogStrategyEvent(%d): %v", i, err)
		}
		last = ev
	}

	recent, err := s.RecentStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("RecentStrategyEvents: %v", err)
	}
	if len(recent) != recentEventsCap {
		t.Fatalf("want the capped length %d, got %d", recentEventsCap, len(recent))
	}
	if recent[len(recent)-1].ID != last.ID {
		t.Fatalf("want the cap to keep the most recent events (tail), last got %+v, want id %s", recent[len(recent)-1], last.ID)
	}
}

// TestResourceUsageNoteRoundTrip proves resourceUsageNote and
// parseResourceUsageNote are actual inverses across the shapes log-usage
// can produce: with cost, without cost, and zero tokens (a real, loggable
// amount, not a sentinel for "unset" at this layer).
func TestResourceUsageNoteRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		tokens  int64
		cost    float64
		hasCost bool
	}{
		{"with cost", "claude-code", 42000, 1.23, true},
		{"without cost", "codex", 900, 0, false},
		{"zero tokens", "claude-code", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := resourceUsageNote(tc.harness, tc.tokens, tc.cost, tc.hasCost)
			got, err := parseResourceUsageNote(note)
			if err != nil {
				t.Fatalf("parseResourceUsageNote(%q): %v", note, err)
			}
			if got.Harness != tc.harness || got.Tokens != tc.tokens || got.HasCost != tc.hasCost {
				t.Fatalf("parseResourceUsageNote(%q) = %+v, want harness=%s tokens=%d hasCost=%v", note, got, tc.harness, tc.tokens, tc.hasCost)
			}
			if tc.hasCost && got.Cost != tc.cost {
				t.Fatalf("parseResourceUsageNote(%q): cost = %v, want %v", note, got.Cost, tc.cost)
			}
			if !tc.hasCost && strings.Contains(note, "cost=") {
				t.Fatalf("resourceUsageNote(%q, hasCost=false) should omit cost entirely, got %q", tc.name, note)
			}
		})
	}
}

// TestParseResourceUsageNoteRejectsMalformed proves the parser errors
// cleanly on shapes it doesn't recognize — freeform prose from the old
// `strategy log -kind=resource_usage -note="..."` path, a hand-edited
// note, or a value that isn't valid for its field — rather than guessing
// or panicking. strategy usage relies on this to skip-and-report instead
// of crashing.
func TestParseResourceUsageNoteRejectsMalformed(t *testing.T) {
	cases := []string{
		"42000 tokens, claude-code",              // freeform prose, pre-log-usage format
		"harness=claude-code",                    // missing tokens
		"tokens=42000",                           // missing harness
		"harness= tokens=42000",                  // empty harness value
		"harness=claude-code tokens=-5",          // negative tokens
		"harness=claude-code tokens=abc",         // non-numeric tokens
		"harness=claude-code tokens=1 cost=abc",  // non-numeric cost
		"harness=claude-code tokens=1 mystery=1", // unknown field
		"",                                       // empty note
	}
	for _, note := range cases {
		if _, err := parseResourceUsageNote(note); err == nil {
			t.Errorf("parseResourceUsageNote(%q): want an error, got nil", note)
		}
	}
}

// TestCmdStrategyLogUsageRejectsMissingRequiredFlags exercises the CLI
// validation path directly (captureOutput, no subprocess) for each
// required flag left unset in turn: -identity, -harness, and -tokens
// (which has no natural zero value for "unset", so it defaults to -1 and
// any negative value is rejected the same way).
func TestCmdStrategyLogUsageRejectsMissingRequiredFlags(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"missing identity", []string{"-db=" + db, "-harness=claude-code", "-tokens=100", st.ID}},
		{"missing harness", []string{"-db=" + db, "-identity=" + alpha.ID, "-tokens=100", st.ID}},
		{"missing tokens", []string{"-db=" + db, "-identity=" + alpha.ID, "-harness=claude-code", st.ID}},
		{"missing strategy id", []string{"-db=" + db, "-identity=" + alpha.ID, "-harness=claude-code", "-tokens=100"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := captureOutput(t, func() int {
				return cmdStrategyLogUsage(tc.args)
			})
			if code == 0 {
				t.Fatalf("cmdStrategyLogUsage(%v): want a non-zero exit code, got 0, output:\n%s", tc.args, out)
			}
		})
	}
}

// TestCmdStrategyUsageMalformedNoteDoesNotCrash proves strategy usage's
// "report the problem, don't hide or crash on it" handling of a note that
// doesn't parse: it must still show the event raw, exclude it from the
// harness totals, report a non-zero exit code, and — the actual point —
// must not panic, while any well-formed events alongside it still total
// correctly.
func TestCmdStrategyUsageMalformedNoteDoesNotCrash(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	// A pre-log-usage freeform note, written the way `strategy log
	// -kind=resource_usage` allowed before this format existed.
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "resource_usage", "roughly 5000 tokens on claude-code"); err != nil {
		t.Fatalf("LogStrategyEvent (freeform): %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "resource_usage", resourceUsageNote("claude-code", 1000, 0.5, true)); err != nil {
		t.Fatalf("LogStrategyEvent (structured): %v", err)
	}

	var out string
	var code int
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("cmdStrategyUsage panicked on a malformed note: %v", r)
			}
		}()
		out, code = captureOutput(t, func() int {
			return cmdStrategyUsage([]string{"-db=" + db, st.ID})
		})
	}()

	if code == 0 {
		t.Fatalf("cmdStrategyUsage: want a non-zero exit code when a note fails to parse, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "roughly 5000 tokens on claude-code") {
		t.Fatalf("cmdStrategyUsage: want the malformed note shown raw, got:\n%s", out)
	}
	if !strings.Contains(out, "skipping") {
		t.Fatalf("cmdStrategyUsage: want the parse failure reported, got:\n%s", out)
	}
	if !strings.Contains(out, "tokens=1000") || !strings.Contains(out, "cost=0.50") {
		t.Fatalf("cmdStrategyUsage: want the well-formed event's totals (tokens=1000, cost=0.50), got:\n%s", out)
	}
}

// TestCloseStrategyRecordsOutcomeAndTimestamp is the regression test for
// the requirement that closing a strategy always leaves a real event
// behind, not just a status flip with no trace: CloseStrategy must set
// status=closed, stamp closed_at, and append a reflection event carrying
// the outcome note.
// TestCreateStrategyLeavesGroupUnset proves a strategy created without a
// group (the common path, and the only path before this feature existed)
// round-trips with an empty Group -- CreateStrategy's signature was
// deliberately left untouched, so this is the default every existing
// call site still gets.
func TestCreateStrategyLeavesGroupUnset(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("ungrouped", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if st.Group != "" {
		t.Fatalf("CreateStrategy: want empty Group by default, got %q", st.Group)
	}
	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Group != "" {
		t.Fatalf("GetStrategy: want empty Group by default, got %q", got.Group)
	}
}

// TestSetStrategyGroupRoundTrips proves SetStrategyGroup actually
// persists: GetStrategy and ListStrategies must both reflect the group
// after it's set, not just the in-process return value.
func TestSetStrategyGroupRoundTrips(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("grouped", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := s.SetStrategyGroup(st.ID, "q3-initiative"); err != nil {
		t.Fatalf("SetStrategyGroup: %v", err)
	}

	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Group != "q3-initiative" {
		t.Fatalf("GetStrategy: want Group %q, got %q", "q3-initiative", got.Group)
	}

	all, err := s.ListStrategies()
	if err != nil {
		t.Fatalf("ListStrategies: %v", err)
	}
	var found bool
	for _, l := range all {
		if l.ID == st.ID {
			found = true
			if l.Group != "q3-initiative" {
				t.Fatalf("ListStrategies: want Group %q, got %q", "q3-initiative", l.Group)
			}
		}
	}
	if !found {
		t.Fatalf("ListStrategies: strategy %s missing from list", st.ID)
	}
}

// TestSetStrategyGroupUnknownIDErrors matches the same not-found pattern
// as SetStrategyStatus -- an unknown id must error, not silently no-op.
func TestSetStrategyGroupUnknownIDErrors(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetStrategyGroup("strategy-does-not-exist", "some-group"); err == nil {
		t.Fatal("SetStrategyGroup: want an error for an unknown strategy id")
	}
}

func TestCloseStrategyRecordsOutcomeAndTimestamp(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "an earlier finding"); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}

	before := time.Now().Add(-time.Second)
	if err := s.CloseStrategy(st.ID, alpha.ID, "confirmed: rowid ordering fixes clock skew here too"); err != nil {
		t.Fatalf("CloseStrategy: %v", err)
	}

	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Status != StrategyClosed {
		t.Fatalf("want status %q after close, got %q", StrategyClosed, got.Status)
	}
	if got.ClosedAt == nil {
		t.Fatal("want a non-nil ClosedAt after close")
	}
	if got.ClosedAt.Before(before) {
		t.Fatalf("ClosedAt %v looks stale (before %v)", got.ClosedAt, before)
	}

	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events (the earlier finding + the close reflection), got %d: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if last.Kind != "reflection" {
		t.Fatalf("want the final event to be kind %q, got %q", "reflection", last.Kind)
	}
	if last.Note != "confirmed: rowid ordering fixes clock skew here too" {
		t.Fatalf("want the reflection event to carry the outcome note, got %q", last.Note)
	}
}
