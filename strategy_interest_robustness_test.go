package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ── shared edge-case string fixtures ────────────────────────────────────
//
// Reused across every field-type test below so "what counts as an edge
// case" is defined once: empty, several-KB (does anything truncate?),
// Unicode (emoji/RTL/combining), embedded newlines (the exact class of bug
// a durability pass already found once for lock resource names — see
// validResourceName in lock.go), CRLF, and SQL-meaningful characters (does
// a parameterized query actually hold, or does something concatenate raw).
func edgeCaseStrings() map[string]string {
	return map[string]string{
		"empty":      "",
		"huge":       strings.Repeat("field-stress-", 500), // ~6.5KB
		"unicode":    "emoji 🎉🔥 RTL مرحبا بالعالم combining e\u0301 CJK 漢字 zero-width\u200b",
		"newline":    "first line\nsecond line\nthird line",
		"crlf":       "first line\r\nsecond line\r\nthird line",
		"sql_meta":   "'; DROP TABLE strategies; --",
		"sql_meta2":  "name\" OR \"1\"=\"1",
		"percent_ws": "100% done -- \t tabs \t and   spaces",
	}
}

// orderedEdgeCaseKeys gives edgeCaseStrings a stable iteration order so
// t.Run subtests have deterministic, reproducible names.
func orderedEdgeCaseKeys() []string {
	m := edgeCaseStrings()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── 1. field-type edge cases ────────────────────────────────────────────

// TestStrategyNameThesisFieldEdgeCases proves CreateStrategy/GetStrategy
// round-trip name and thesis exactly for every edge-case shape, with no
// silent truncation, mangling, or SQL-special-character misbehavior --
// confirming the parameterized INSERT/SELECT actually holds rather than
// assuming it.
func TestStrategyNameThesisFieldEdgeCases(t *testing.T) {
	s := newTestStore(t)
	cases := edgeCaseStrings()
	for _, key := range orderedEdgeCaseKeys() {
		val := cases[key]
		t.Run(key, func(t *testing.T) {
			st, err := s.CreateStrategy(val, val)
			if err != nil {
				t.Fatalf("CreateStrategy(%q): %v", key, err)
			}
			got, err := s.GetStrategy(st.ID)
			if err != nil {
				t.Fatalf("GetStrategy: %v", err)
			}
			if got.Name != val {
				t.Fatalf("Name round-trip: got %d bytes, want %d bytes (mismatch)", len(got.Name), len(val))
			}
			if got.Thesis != val {
				t.Fatalf("Thesis round-trip: got %d bytes, want %d bytes (mismatch)", len(got.Thesis), len(val))
			}
		})
	}
}

// TestStrategyEventNoteFieldEdgeCases is the same round-trip proof for
// LogStrategyEvent's note field via ListStrategyEvents.
func TestStrategyEventNoteFieldEdgeCases(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "field edge cases on note")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	cases := edgeCaseStrings()
	// "empty" is skipped here: LogStrategyEvent's own CLI wrapper requires
	// -note non-empty (cmdStrategyLog), but the Store method itself has no
	// such guard -- both are worth confirming explicitly rather than
	// assumed, so empty is handled in its own case below instead of the
	// loop.
	for _, key := range orderedEdgeCaseKeys() {
		val := cases[key]
		t.Run(key, func(t *testing.T) {
			ev, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", val)
			if err != nil {
				t.Fatalf("LogStrategyEvent(%q): %v", key, err)
			}
			events, err := s.ListStrategyEvents(st.ID)
			if err != nil {
				t.Fatalf("ListStrategyEvents: %v", err)
			}
			var found *StrategyEvent
			for _, e := range events {
				if e.ID == ev.ID {
					found = e
				}
			}
			if found == nil {
				t.Fatalf("event %s missing from ListStrategyEvents", ev.ID)
			}
			if found.Note != val {
				t.Fatalf("Note round-trip: got %d bytes, want %d bytes (mismatch)", len(found.Note), len(val))
			}
		})
	}
}

// TestStrategyGroupLabelFieldEdgeCases is the round-trip proof for
// SetStrategyGroup's group label via GetStrategy.
func TestStrategyGroupLabelFieldEdgeCases(t *testing.T) {
	s := newTestStore(t)
	cases := edgeCaseStrings()
	for _, key := range orderedEdgeCaseKeys() {
		if key == "empty" {
			continue // empty is exactly the "unset" sentinel -- covered by its own test below
		}
		val := cases[key]
		t.Run(key, func(t *testing.T) {
			st, err := s.CreateStrategy("probe-"+key, "thesis")
			if err != nil {
				t.Fatalf("CreateStrategy: %v", err)
			}
			if err := s.SetStrategyGroup(st.ID, val); err != nil {
				t.Fatalf("SetStrategyGroup(%q): %v", key, err)
			}
			got, err := s.GetStrategy(st.ID)
			if err != nil {
				t.Fatalf("GetStrategy: %v", err)
			}
			if got.Group != val {
				t.Fatalf("Group round-trip: got %d bytes, want %d bytes (mismatch)", len(got.Group), len(val))
			}
		})
	}
}

// TestInterestLabelFieldEdgeCases is the round-trip proof for
// CreateInterest's label via ListInterests. Each case uses a distinct
// resource name (the label edge-case strings themselves aren't valid
// resource names -- newlines are rejected there by design) so
// CreateInterest's own identity+resource idempotency doesn't collapse
// separate subtests into one interest.
func TestInterestLabelFieldEdgeCases(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	cases := edgeCaseStrings()
	for i, key := range orderedEdgeCaseKeys() {
		val := cases[key]
		t.Run(key, func(t *testing.T) {
			resource := fmt.Sprintf("res-label-edge-%d", i)
			in, err := s.CreateInterest(alpha.ID, resource, val)
			if err != nil {
				t.Fatalf("CreateInterest(%q): %v", key, err)
			}
			all, err := s.ListInterests()
			if err != nil {
				t.Fatalf("ListInterests: %v", err)
			}
			var found *Interest
			for _, x := range all {
				if x.ID == in.ID {
					found = x
				}
			}
			if found == nil {
				t.Fatalf("interest %s missing from ListInterests", in.ID)
			}
			if found.Label != val {
				t.Fatalf("Label round-trip: got %d bytes, want %d bytes (mismatch)", len(found.Label), len(val))
			}
		})
	}
}

// TestSQLMetaCharactersDoNotCorruptTheDatabase is the direct proof that
// every one of these fields goes through a parameterized query, not string
// concatenation: writing literal SQL-meaningful text (a DROP TABLE
// attempt, a quote-based injection attempt) into name/thesis/note/group/
// label must store it as inert data, and the schema and every other row
// must remain completely unaffected afterward.
func TestSQLMetaCharactersDoNotCorruptTheDatabase(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	// A strategy created before the attack strings, used as the "did the
	// schema survive" canary.
	canary, err := s.CreateStrategy("canary", "must still exist afterward")
	if err != nil {
		t.Fatalf("CreateStrategy(canary): %v", err)
	}

	attacks := []string{
		"'; DROP TABLE strategies; --",
		"'; DROP TABLE strategy_events; --",
		"x' OR '1'='1",
		"Robert'); DROP TABLE interests;--",
	}
	for _, atk := range attacks {
		st, err := s.CreateStrategy(atk, atk)
		if err != nil {
			t.Fatalf("CreateStrategy(attack string %q): %v", atk, err)
		}
		if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", atk); err != nil {
			t.Fatalf("LogStrategyEvent(attack string %q): %v", atk, err)
		}
		if err := s.SetStrategyGroup(st.ID, atk); err != nil {
			t.Fatalf("SetStrategyGroup(attack string %q): %v", atk, err)
		}
		got, err := s.GetStrategy(st.ID)
		if err != nil {
			t.Fatalf("GetStrategy after attack string: %v", err)
		}
		if got.Name != atk || got.Thesis != atk || got.Group != atk {
			t.Fatalf("attack string %q was not stored literally: got name=%q thesis=%q group=%q", atk, got.Name, got.Thesis, got.Group)
		}
	}

	// The canary, and every table, must still be intact: the schema was
	// never actually executed as SQL.
	stillThere, err := s.GetStrategy(canary.ID)
	if err != nil {
		t.Fatalf("GetStrategy(canary) after attack strings: %v", err)
	}
	if stillThere == nil {
		t.Fatal("canary strategy vanished -- a SQL injection attempt actually executed")
	}
	all, err := s.ListStrategies()
	if err != nil {
		t.Fatalf("ListStrategies after attack strings: %v", err)
	}
	if len(all) != len(attacks)+1 {
		t.Fatalf("want %d strategies (canary + %d attacks), got %d", len(attacks)+1, len(attacks), len(all))
	}
}

// TestNewlineInNoteDoesNotCorruptStrategyShowDisplay is the regression test
// for a real bug this test suite found: a note containing embedded
// newlines printed raw in `strategy show`, breaking the one-event-per-line
// display the exact same way a durability pass once found an embedded
// newline could break `lock list` (see validResourceName in lock.go).
// escapeForSingleLineDisplay (strategy.go) now escapes newlines for
// display, so every line of the note's content should render as one
// logical event line with literal "\n" markers, and no bare unlabeled
// continuation line should ever appear.
func TestNewlineInNoteDoesNotCorruptStrategyShowDisplay(t *testing.T) {
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
	st, err := s.CreateStrategy("probe", "does a multi-line note corrupt strategy show")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	multiline := "line one of the note\nline two of the note\nline three of the note"
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", multiline); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}
	// A second, single-line event logged right after -- if the newline
	// above corrupts the display, this sentinel line's own "by=" marker
	// would end up on the wrong logical line.
	sentinelNote := "SENTINEL-EVENT-AFTER-MULTILINE"
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", sentinelNote); err != nil {
		t.Fatalf("LogStrategyEvent(sentinel): %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdStrategyShow([]string{"-db=" + db, st.ID})
	})
	if code != 0 {
		t.Fatalf("cmdStrategyShow: exit %d, output:\n%s", code, out)
	}
	// Fixed behavior: no line of the output should be a bare, unlabeled
	// continuation of the note (that was the actual corruption) -- every
	// line belonging to the multi-line note must still carry the event's
	// fixed prefix, because the newlines were escaped, not printed raw.
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "line two of the note" || trimmed == "line three of the note" {
			t.Fatalf("a note's embedded newline still produced a bare unlabeled continuation line %q -- escapeForSingleLineDisplay isn't being applied here, got:\n%s", trimmed, out)
		}
	}
	// The escaped form (literal backslash-n) must still carry the note's
	// actual content, on the SAME line as the rest of that event's fields.
	wantEscaped := "line one of the note\\nline two of the note\\nline three of the note"
	line := lineContaining(out, "line one of the note")
	if !strings.Contains(line, wantEscaped) {
		t.Fatalf("want the multi-line note escaped onto one line as %q, got line:\n%s", wantEscaped, line)
	}
	if !strings.Contains(line, "by=") {
		t.Fatalf("the escaped note's line lost its event prefix, got:\n%s", line)
	}
	if !strings.Contains(out, sentinelNote) {
		t.Fatalf("the sentinel event logged after the multi-line note is missing from strategy show output entirely, got:\n%s", out)
	}
}

// TestNewlineInGroupDoesNotCorruptStrategyListDisplay is the same
// regression test for `strategy list`'s group column -- one strategy per
// line, group_name printed inline mid-row (followed by "created="), so an
// unescaped embedded newline there is the more severe case: it doesn't
// just misalign a line, it actually orphans the created= timestamp onto
// what looks like an unrelated line.
func TestNewlineInGroupDoesNotCorruptStrategyListDisplay(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := s.SetStrategyGroup(st.ID, "group-line-one\ngroup-line-two"); err != nil {
		t.Fatalf("SetStrategyGroup: %v", err)
	}
	sentinel, err := s.CreateStrategy("sentinel-strategy", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy(sentinel): %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdStrategyList([]string{"-db=" + db})
	})
	if code != 0 {
		t.Fatalf("cmdStrategyList: exit %d, output:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("a group label's embedded newline split `strategy list`'s output across %d lines instead of 2 (one per strategy) -- escapeForSingleLineDisplay isn't being applied to the group column, got:\n%s", len(lines), out)
	}
	groupLine := lineContaining(out, st.ID)
	if !strings.Contains(groupLine, "group-line-one\\ngroup-line-two") {
		t.Fatalf("want the group's embedded newline escaped inline, got line:\n%s", groupLine)
	}
	if !strings.Contains(groupLine, "created=") {
		t.Fatalf("the created= field was orphaned off this strategy's row, got:\n%s", out)
	}
	if !strings.Contains(out, sentinel.ID) {
		t.Fatalf("sentinel strategy missing from strategy list output, got:\n%s", out)
	}
}

// TestUnicodeFieldsStayLegibleInStrategyShowAndList proves Unicode content
// (emoji, RTL text, combining characters, CJK) round-trips byte-for-byte
// through both storage and the CLI's plain-text display, and that
// cmdStrategyShow/cmdStrategyList don't panic or mangle it.
func TestUnicodeFieldsStayLegibleInStrategyShowAndList(t *testing.T) {
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
	name := "emoji-strategy-🎉🔥"
	thesis := "RTL: مرحبا بالعالم -- CJK: 漢字試験 -- combining: e\u0301"
	st, err := s.CreateStrategy(name, thesis)
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := s.SetStrategyGroup(st.ID, "grupo-🚀"); err != nil {
		t.Fatalf("SetStrategyGroup: %v", err)
	}
	note := "found it: 漢字 renders fine, 🔥 emoji fine, combining é fine"
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", note); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}

	showOut, code := captureOutput(t, func() int { return cmdStrategyShow([]string{"-db=" + db, st.ID}) })
	if code != 0 {
		t.Fatalf("cmdStrategyShow: exit %d, output:\n%s", code, showOut)
	}
	for _, want := range []string{name, thesis, "grupo-🚀", note} {
		if !strings.Contains(showOut, want) {
			t.Fatalf("cmdStrategyShow output missing unicode content %q, got:\n%s", want, showOut)
		}
	}

	listOut, code := captureOutput(t, func() int { return cmdStrategyList([]string{"-db=" + db}) })
	if code != 0 {
		t.Fatalf("cmdStrategyList: exit %d, output:\n%s", code, listOut)
	}
	if !strings.Contains(listOut, name) || !strings.Contains(listOut, "grupo-🚀") {
		t.Fatalf("cmdStrategyList output missing unicode content, got:\n%s", listOut)
	}
}

// ── 2. strategy log -kind= edge cases ───────────────────────────────────

// TestValidStrategyEventKindsExactSet re-derives the currently-valid kind
// set directly from validStrategyEventKinds (not a hardcoded list that
// could go stale) and pins it down: exactly these six, no more, no fewer --
// core and external_signal in particular must not be present.
func TestValidStrategyEventKindsExactSet(t *testing.T) {
	want := map[string]bool{
		"step_started":   true,
		"step_completed": true,
		"finding":        true,
		"decision":       true,
		"reflection":     true,
		"resource_usage": true,
	}
	if len(validStrategyEventKinds) != len(want) {
		t.Fatalf("validStrategyEventKinds has %d entries, want %d: got %v", len(validStrategyEventKinds), len(want), validStrategyEventKinds)
	}
	for k := range want {
		if !validStrategyEventKinds[k] {
			t.Errorf("validStrategyEventKinds missing expected kind %q", k)
		}
	}
	for _, dead := range []string{"core", "external_signal"} {
		if validStrategyEventKinds[dead] {
			t.Errorf("validStrategyEventKinds still contains cut kind %q", dead)
		}
	}
}

// TestAllValidKindsAcceptBoundaryShapedNotes logs every currently-valid
// kind (derived from the map, so this test tracks the map rather than
// duplicating it) with both an empty and a huge note, confirming
// LogStrategyEvent's own kind check is the only gate -- no kind is
// secretly pickier about note shape than another.
func TestAllValidKindsAcceptBoundaryShapedNotes(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "every valid kind, boundary notes")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	huge := strings.Repeat("resource-usage-note-stress-", 400)
	var kinds []string
	for k := range validStrategyEventKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for _, note := range []string{"", huge} {
			label := kind + "/len=" + strconv.Itoa(len(note))
			t.Run(label, func(t *testing.T) {
				ev, err := s.LogStrategyEvent(st.ID, alpha.ID, kind, note)
				if err != nil {
					t.Fatalf("LogStrategyEvent(kind=%q, note len=%d): %v", kind, len(note), err)
				}
				if ev.Kind != kind || ev.Note != note {
					t.Fatalf("LogStrategyEvent(kind=%q): got kind=%q note len=%d, want kind=%q note len=%d", kind, ev.Kind, len(ev.Note), kind, len(note))
				}
			})
		}
	}
}

// TestStrategyEventKindRejectsCaseAndWhitespaceVariants proves kind
// matching is an exact, case-sensitive map lookup -- not any kind of loose
// or normalized comparison that would silently accept "Step_started" or a
// trailing-space variant as if it were "step_started".
func TestStrategyEventKindRejectsCaseAndWhitespaceVariants(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "case/whitespace kind variants")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	variants := []string{
		"Step_started", "STEP_STARTED", "step_Started",
		"step_started ", " step_started", "step_started\n", "step_started\t",
		"Finding", "FINDING", "Decision", "REFLECTION", "Resource_Usage",
	}
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			if _, err := s.LogStrategyEvent(st.ID, alpha.ID, v, "note"); err == nil {
				t.Fatalf("LogStrategyEvent(kind=%q): want an error, a loose comparison silently accepted it", v)
			}
		})
	}
	// Confirm nothing was actually written by any of the rejected variants.
	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want 0 events after only rejected kind variants, got %d: %+v", len(events), events)
	}
}

// TestStrategyEventKindErrorTextMatchesTheActualValidSet is a staleness
// guard: LogStrategyEvent's error message spells out the valid kinds as a
// separate hardcoded string (a second source of truth alongside the map).
// This confirms the two never drift apart -- in particular that neither
// "core" nor "external_signal" (both cut) lingers in the error text a human
// or an agent would actually read.
func TestStrategyEventKindErrorTextMatchesTheActualValidSet(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	_, err = s.LogStrategyEvent(st.ID, "whoever", "not-a-real-kind", "note")
	if err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	msg := err.Error()
	idx := strings.Index(msg, "(want ")
	if idx == -1 {
		t.Fatalf("error text %q doesn't contain the expected \"(want ...)\" clause", msg)
	}
	clause := strings.TrimSuffix(msg[idx+len("(want "):], ")")
	listed := strings.Split(clause, "|")

	want := make([]string, 0, len(validStrategyEventKinds))
	for k := range validStrategyEventKinds {
		want = append(want, k)
	}
	sort.Strings(want)
	sort.Strings(listed)
	if len(listed) != len(want) {
		t.Fatalf("error text lists %v, want exactly %v", listed, want)
	}
	for i := range want {
		if listed[i] != want[i] {
			t.Fatalf("error text lists %v, want exactly %v", listed, want)
		}
	}
}

// ── 3. strategy list -group= and grouping at scale ──────────────────────

// TestStrategyListGroupFilteringAtScale creates 24 strategies across 4
// groups plus a block of ungrouped strategies, then confirms
// cmdStrategyList's -group filter (the actual CLI-level filtering logic
// under test, not just the store layer) returns exactly the matching set
// at each group, no more and no fewer, even with dozens of strategies in
// play.
func TestStrategyListGroupFilteringAtScale(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	groups := []string{"alpha-initiative", "beta-initiative", "gamma-initiative", "delta-initiative"}
	perGroup := 5
	const ungrouped = 4
	wantIDs := map[string]map[string]bool{} // group -> set of ids
	for _, g := range groups {
		wantIDs[g] = map[string]bool{}
		for i := 0; i < perGroup; i++ {
			st, err := s.CreateStrategy(fmt.Sprintf("%s-strategy-%d", g, i), "thesis")
			if err != nil {
				t.Fatalf("CreateStrategy: %v", err)
			}
			if err := s.SetStrategyGroup(st.ID, g); err != nil {
				t.Fatalf("SetStrategyGroup: %v", err)
			}
			wantIDs[g][st.ID] = true
		}
	}
	var ungroupedIDs []string
	for i := 0; i < ungrouped; i++ {
		st, err := s.CreateStrategy(fmt.Sprintf("ungrouped-%d", i), "thesis")
		if err != nil {
			t.Fatalf("CreateStrategy(ungrouped): %v", err)
		}
		ungroupedIDs = append(ungroupedIDs, st.ID)
	}

	for _, g := range groups {
		g := g
		t.Run(g, func(t *testing.T) {
			out, code := captureOutput(t, func() int {
				return cmdStrategyList([]string{"-db=" + db, "-group=" + g})
			})
			if code != 0 {
				t.Fatalf("cmdStrategyList -group=%s: exit %d, output:\n%s", g, code, out)
			}
			for id := range wantIDs[g] {
				if !strings.Contains(out, id) {
					t.Errorf("group %s: missing expected strategy %s from filtered output", g, id)
				}
			}
			for _, other := range groups {
				if other == g {
					continue
				}
				for id := range wantIDs[other] {
					if strings.Contains(out, id) {
						t.Errorf("group %s: filtered output leaked strategy %s from group %s", g, id, other)
					}
				}
			}
			for _, id := range ungroupedIDs {
				if strings.Contains(out, id) {
					t.Errorf("group %s: filtered output leaked an ungrouped strategy %s", g, id)
				}
			}
			lines := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1
			if lines != perGroup {
				t.Errorf("group %s: want exactly %d lines of output, got %d:\n%s", g, perGroup, lines, out)
			}
		})
	}
}

// TestSetStrategyGroupClearActuallyUnsetsIt proves the full lifecycle:
// set a group, change it to a different value, then clear it back to "" --
// and that clearing genuinely restores the "never grouped" display
// convention (a plain "-"), not some other unexpected empty-but-different
// state, and that ListStrategies filtering by the old group no longer
// matches.
func TestSetStrategyGroupClearActuallyUnsetsIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	st, err := s.CreateStrategy("probe", "thesis")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := s.SetStrategyGroup(st.ID, "first-group"); err != nil {
		t.Fatalf("SetStrategyGroup(first): %v", err)
	}
	if err := s.SetStrategyGroup(st.ID, "second-group"); err != nil {
		t.Fatalf("SetStrategyGroup(second): %v", err)
	}
	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if got.Group != "second-group" {
		t.Fatalf("want Group %q after re-set, got %q", "second-group", got.Group)
	}

	if err := s.SetStrategyGroup(st.ID, ""); err != nil {
		t.Fatalf("SetStrategyGroup(clear): %v", err)
	}
	got, err = s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy after clear: %v", err)
	}
	if got.Group != "" {
		t.Fatalf("want empty Group after explicit clear, got %q", got.Group)
	}

	showOut, code := captureOutput(t, func() int { return cmdStrategyShow([]string{"-db=" + db, st.ID}) })
	if code != 0 {
		t.Fatalf("cmdStrategyShow: exit %d, output:\n%s", code, showOut)
	}
	if !strings.Contains(showOut, "group:   -") {
		t.Fatalf("strategy show should display \"-\" for an explicitly-cleared group (same convention as never-grouped), got:\n%s", showOut)
	}

	listOut, code := captureOutput(t, func() int { return cmdStrategyList([]string{"-db=" + db, "-group=second-group"}) })
	if code != 0 {
		t.Fatalf("cmdStrategyList: exit %d, output:\n%s", code, listOut)
	}
	if strings.Contains(listOut, st.ID) {
		t.Fatalf("filtering by the old group value should no longer match after clearing, got:\n%s", listOut)
	}
	if !strings.Contains(listOut, "no strategies") {
		t.Fatalf("want \"no strategies\" once the only match was cleared, got:\n%s", listOut)
	}
}

// ── 4. RecentStrategyEvents / RecentPropagationsForIdentity edge cases ──

// TestRecentStrategyEventsCutsAtTheMostRecentStepCompleted is the
// adversarial version of the existing single-boundary test: multiple
// step_completed events in one strategy must cut off at the LAST one, not
// the first.
func TestRecentStrategyEventsCutsAtTheMostRecentStepCompleted(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "multiple step_completed events")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	mustLog := func(kind, note string) *StrategyEvent {
		ev, err := s.LogStrategyEvent(st.ID, alpha.ID, kind, note)
		if err != nil {
			t.Fatalf("LogStrategyEvent(%s, %s): %v", kind, note, err)
		}
		return ev
	}
	mustLog("step_started", "step 1 started")
	mustLog("finding", "step 1 finding")
	firstBoundary := mustLog("step_completed", "step 1 done")
	mustLog("step_started", "step 2 started")
	mustLog("finding", "step 2 finding")
	secondBoundary := mustLog("step_completed", "step 2 done")
	afterSecond := mustLog("finding", "step 3 in progress")

	recent, err := s.RecentStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("RecentStrategyEvents: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("want 2 recent events (2nd boundary + 1 after), got %d: %+v", len(recent), recent)
	}
	if recent[0].ID != secondBoundary.ID {
		t.Fatalf("recent[0]: want the SECOND (most recent) step_completed %s, got %s (%s)", secondBoundary.ID, recent[0].ID, recent[0].Note)
	}
	if recent[1].ID != afterSecond.ID {
		t.Fatalf("recent[1]: want %s, got %s", afterSecond.ID, recent[1].ID)
	}
	for _, r := range recent {
		if r.ID == firstBoundary.ID {
			t.Fatalf("recent events wrongly include the FIRST (stale) step_completed boundary %s", firstBoundary.ID)
		}
	}
}

// TestRecentStrategyEventsCapCountsFromTheEndEvenPastTheBoundary documents
// (not "fixes" -- this is the behavior RecentStrategyEvents' own doc
// comment states explicitly: "Either way the window is capped at
// recentEventsCap events, counting from the end") what happens when more
// than recentEventsCap events occur AFTER the step_completed boundary: the
// cap wins and the boundary event itself can be pushed out of the window.
// This pins that documented tradeoff down as a real, observed behavior.
func TestRecentStrategyEventsCapCountsFromTheEndEvenPastTheBoundary(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	st, err := s.CreateStrategy("probe", "cap past the boundary")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	boundary, err := s.LogStrategyEvent(st.ID, alpha.ID, "step_completed", "boundary")
	if err != nil {
		t.Fatalf("LogStrategyEvent(boundary): %v", err)
	}
	const postBoundary = recentEventsCap + 3
	var last *StrategyEvent
	for i := 0; i < postBoundary; i++ {
		ev, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", fmt.Sprintf("post-boundary finding #%d", i))
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
		t.Fatalf("want the cap to keep the most recent events (tail), got last=%+v want id %s", recent[len(recent)-1], last.ID)
	}
	var boundaryStillPresent bool
	for _, r := range recent {
		if r.ID == boundary.ID {
			boundaryStillPresent = true
		}
	}
	if boundaryStillPresent {
		t.Fatalf("with %d events after the boundary (cap=%d), the boundary was expected to be pushed out by the tail-cap -- got it still present, so either the cap changed or events count is off", postBoundary, recentEventsCap)
	}
}

// TestRecentStrategyEventsZeroEvents proves a freshly created strategy with
// no events at all returns an empty (not nil-panicking, not erroring)
// slice from RecentStrategyEvents.
func TestRecentStrategyEventsZeroEvents(t *testing.T) {
	s := newTestStore(t)
	st, err := s.CreateStrategy("probe", "no events at all")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	recent, err := s.RecentStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("RecentStrategyEvents: %v", err)
	}
	if len(recent) != 0 {
		t.Fatalf("want 0 recent events for a strategy with no log at all, got %d: %+v", len(recent), recent)
	}
}

// TestStrategyNextOnClosedStrategyStillWorks proves `strategy next` doesn't
// need to special-case a closed strategy to behave sensibly: it should
// still report status=closed, the closed timestamp, and a coherent recap
// (including the close reflection event CloseStrategy itself appends) --
// not error out or print something nonsensical just because the strategy
// is done.
func TestStrategyNextOnClosedStrategyStillWorks(t *testing.T) {
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
	st, err := s.CreateStrategy("probe", "closed strategy, next should still work")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if _, err := s.LogStrategyEvent(st.ID, alpha.ID, "finding", "final finding before close"); err != nil {
		t.Fatalf("LogStrategyEvent: %v", err)
	}
	outcome := "closed: everything confirmed"
	if err := s.CloseStrategy(st.ID, alpha.ID, outcome); err != nil {
		t.Fatalf("CloseStrategy: %v", err)
	}

	out, code := captureOutput(t, func() int { return cmdStrategyNext([]string{"-db=" + db, st.ID}) })
	if code != 0 {
		t.Fatalf("cmdStrategyNext on a closed strategy: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "status:  closed") {
		t.Fatalf("strategy next should report status closed, got:\n%s", out)
	}
	if !strings.Contains(out, "closed:") {
		t.Fatalf("strategy next should print the closed timestamp, got:\n%s", out)
	}
	if !strings.Contains(out, outcome) {
		t.Fatalf("strategy next should recap the close outcome, got:\n%s", out)
	}
}

// TestStrategyNextManyIdentitiesInterleavedStaysCoherent logs events from
// 12 different identities interleaved on one strategy and confirms
// `strategy next`'s recap attributes every event to the correct identity --
// no misattribution from interleaving.
func TestStrategyNextManyIdentitiesInterleavedStaysCoherent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(db)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const n = 12
	identities := make([]*Identity, n)
	for i := 0; i < n; i++ {
		id, err := s.CreateIdentity(fmt.Sprintf("agent-%d", i))
		if err != nil {
			t.Fatalf("CreateIdentity(%d): %v", i, err)
		}
		identities[i] = id
	}
	st, err := s.CreateStrategy("probe", "many interleaved identities")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	// Interleave: each identity logs a step_started then, at the very end,
	// identity 0 logs the one step_completed boundary. The recap should
	// show only the boundary + whatever comes after -- and every note in
	// the recap window must be attributed to the identity that actually
	// logged it.
	type expected struct {
		note       string
		identityID string
	}
	var wantAfterBoundary []expected
	for i := 0; i < n; i++ {
		note := fmt.Sprintf("finding-from-agent-%d", i)
		if _, err := s.LogStrategyEvent(st.ID, identities[i].ID, "finding", note); err != nil {
			t.Fatalf("LogStrategyEvent(%d): %v", i, err)
		}
	}
	boundaryNote := "boundary logged by agent-0"
	if _, err := s.LogStrategyEvent(st.ID, identities[0].ID, "step_completed", boundaryNote); err != nil {
		t.Fatalf("LogStrategyEvent(boundary): %v", err)
	}
	for i := 0; i < n; i++ {
		note := fmt.Sprintf("post-boundary-finding-from-agent-%d", i)
		if _, err := s.LogStrategyEvent(st.ID, identities[i].ID, "finding", note); err != nil {
			t.Fatalf("LogStrategyEvent(post, %d): %v", i, err)
		}
		wantAfterBoundary = append(wantAfterBoundary, expected{note, identities[i].ID})
	}

	out, code := captureOutput(t, func() int { return cmdStrategyNext([]string{"-db=" + db, st.ID}) })
	if code != 0 {
		t.Fatalf("cmdStrategyNext: exit %d, output:\n%s", code, out)
	}
	// The recap is capped at recentEventsCap, counting from the end -- so
	// only the tail of wantAfterBoundary (plus however much of the
	// boundary survives) is guaranteed present. Check the events that must
	// be within that trailing window are attributed correctly.
	tailStart := len(wantAfterBoundary) - recentEventsCap
	if tailStart < 0 {
		tailStart = 0
	}
	for _, exp := range wantAfterBoundary[tailStart:] {
		line := lineContaining(out, exp.note)
		if line == "" {
			t.Fatalf("recap missing expected post-boundary note %q entirely, got:\n%s", exp.note, out)
		}
		if !strings.Contains(line, "by="+exp.identityID) {
			t.Fatalf("recap line for %q misattributed identity, want by=%s, got line:\n%s", exp.note, exp.identityID, line)
		}
	}
}

// lineContaining returns the first line of out containing needle, or "".
func lineContaining(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

// TestRecentPropagationsForIdentityLastRowIDEdgeCases exercises lastRowID
// values that don't correspond to any real row: 0 (the documented "from
// the start" sentinel), negative, and a value far beyond the current max
// rowid -- none of these should crash or misbehave.
func TestRecentPropagationsForIdentityLastRowIDEdgeCases(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.CreateInterest(beta.ID, "watched-res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}
	if _, err := s.AcquireLock("watched-res", alpha.ID, "editing", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := s.ReleaseLock("watched-res", alpha.ID, "done", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	// lastRowID=0: from the very start, must return the one delivery.
	recent, maxID, err := s.RecentPropagationsForIdentity(beta.ID, 0, 50)
	if err != nil {
		t.Fatalf("RecentPropagationsForIdentity(lastRowID=0): %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("lastRowID=0: want 1 delivery, got %d", len(recent))
	}
	if maxID != recent[0].RowID {
		t.Fatalf("lastRowID=0: returned maxRowID %d doesn't match the one delivery's RowID %d", maxID, recent[0].RowID)
	}

	// lastRowID negative: rowid > -1 is true for every real row (rowid
	// always starts at 1), so this should behave the same as lastRowID=0 --
	// not crash, not error.
	recentNeg, _, err := s.RecentPropagationsForIdentity(beta.ID, -1, 50)
	if err != nil {
		t.Fatalf("RecentPropagationsForIdentity(lastRowID=-1): %v", err)
	}
	if len(recentNeg) != 1 {
		t.Fatalf("lastRowID=-1: want 1 delivery (same as lastRowID=0), got %d", len(recentNeg))
	}

	// lastRowID far beyond the current max: no rows should match, and
	// maxRowID should just echo back lastRowID unchanged (nothing newer
	// was found to advance it).
	recentFar, maxFar, err := s.RecentPropagationsForIdentity(beta.ID, 999999, 50)
	if err != nil {
		t.Fatalf("RecentPropagationsForIdentity(lastRowID=999999): %v", err)
	}
	if len(recentFar) != 0 {
		t.Fatalf("lastRowID beyond current max: want 0 deliveries, got %d: %+v", len(recentFar), recentFar)
	}
	if maxFar != 999999 {
		t.Fatalf("lastRowID beyond current max: want maxRowID to echo back unchanged (999999), got %d", maxFar)
	}
}

// TestRecentPropagationsForIdentityLimitEdgeCases exercises limit=0 and a
// negative limit. limit=0 returns zero rows (plain SQL LIMIT 0 semantics).
// A negative limit is the regression case: SQLite's own documented
// behavior for a negative LIMIT is "no upper bound," which would silently
// turn a bounded polling primitive into an unbounded dump -- this test
// confirms RecentPropagationsForIdentity's own clamp (limit < 0 -> 0)
// actually holds, with a real query, rather than assuming it.
func TestRecentPropagationsForIdentityLimitEdgeCases(t *testing.T) {
	s := newTestStore(t)
	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.CreateInterest(beta.ID, "watched-res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}
	// Generate several deliveries: acquire+release three different watched
	// resources, each interest-matched.
	for _, res := range []string{"watched-res", "watched-res", "watched-res"} {
		if _, err := s.AcquireLock(res, alpha.ID, "editing", "", 0); err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		if err := s.ReleaseLock(res, alpha.ID, "done", false); err != nil {
			t.Fatalf("ReleaseLock: %v", err)
		}
	}
	all, _, err := s.RecentPropagationsForIdentity(beta.ID, 0, 1000)
	if err != nil {
		t.Fatalf("RecentPropagationsForIdentity (baseline): %v", err)
	}
	if len(all) == 0 {
		t.Fatal("expected at least one delivery to exist as a baseline for this test")
	}

	zero, _, err := s.RecentPropagationsForIdentity(beta.ID, 0, 0)
	if err != nil {
		t.Fatalf("RecentPropagationsForIdentity(limit=0): %v", err)
	}
	if len(zero) != 0 {
		t.Fatalf("limit=0: want 0 deliveries, got %d", len(zero))
	}

	for _, negLimit := range []int{-1, -1000} {
		neg, _, err := s.RecentPropagationsForIdentity(beta.ID, 0, negLimit)
		if err != nil {
			t.Fatalf("RecentPropagationsForIdentity(limit=%d): %v", negLimit, err)
		}
		if len(neg) != 0 {
			t.Fatalf("limit=%d: want 0 deliveries (clamped, not SQLite's \"no upper bound\" semantics), got %d out of a baseline total of %d", negLimit, len(neg), len(all))
		}
	}
}

// ── 6. concurrency ───────────────────────────────────────────────────────

// TestConcurrentLogStrategyEventNoLostWrites hits one real strategy with
// many identities logging events concurrently from real goroutines against
// a real on-disk SQLite file -- not sequential calls, which would never
// exercise the actual contention this project's own store.exec retry loop
// exists for. Every write must land: no lost writes, no duplicate/
// corrupted rows, and the append-only property (every row distinct,
// nothing overwritten) must hold under real concurrent pressure.
func TestConcurrentLogStrategyEventNoLostWrites(t *testing.T) {
	db := filepath.Join(t.TempDir(), "concurrent-events.db")
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	const identities = 15
	const eventsPerIdentity = 12
	ids := make([]*Identity, identities)
	for i := range ids {
		id, err := s.CreateIdentity(fmt.Sprintf("agent-%d", i))
		if err != nil {
			t.Fatalf("CreateIdentity(%d): %v", i, err)
		}
		ids[i] = id
	}
	st, err := s.CreateStrategy("probe", "concurrent writers, one strategy")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, identities*eventsPerIdentity)
	for i := 0; i < identities; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < eventsPerIdentity; j++ {
				note := fmt.Sprintf("agent=%d seq=%d", i, j)
				if _, err := s.LogStrategyEvent(st.ID, ids[i].ID, "finding", note); err != nil {
					errCh <- fmt.Errorf("agent %d seq %d: %w", i, j, err)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent LogStrategyEvent error: %v", err)
	}

	events, err := s.ListStrategyEvents(st.ID)
	if err != nil {
		t.Fatalf("ListStrategyEvents: %v", err)
	}
	wantTotal := identities * eventsPerIdentity
	if len(events) != wantTotal {
		t.Fatalf("want %d total events (no lost writes), got %d", wantTotal, len(events))
	}

	// No duplicate/corrupted rows: every event ID unique, every note
	// matches the "agent=%d seq=%d" shape exactly once.
	seenIDs := map[string]bool{}
	seenNotes := map[string]bool{}
	for _, ev := range events {
		if seenIDs[ev.ID] {
			t.Fatalf("duplicate event ID %s -- append-only guarantee violated under concurrency", ev.ID)
		}
		seenIDs[ev.ID] = true
		if seenNotes[ev.Note] {
			t.Fatalf("duplicate note %q -- two events collapsed into one under concurrency", ev.Note)
		}
		seenNotes[ev.Note] = true
		if ev.Kind != "finding" {
			t.Fatalf("event %s has corrupted kind %q, want %q", ev.ID, ev.Kind, "finding")
		}
	}
	for i := 0; i < identities; i++ {
		for j := 0; j < eventsPerIdentity; j++ {
			want := fmt.Sprintf("agent=%d seq=%d", i, j)
			if !seenNotes[want] {
				t.Errorf("missing expected note %q -- a write was lost under concurrency", want)
			}
		}
	}
}

// TestConcurrentSetStrategyGroupAndStatus hits one strategy with concurrent
// SetStrategyGroup and SetStrategyStatus calls from different goroutines.
// Neither call should ever error (both are unconditional UPDATEs on an id
// that does exist throughout), and the final state, whichever write
// happened to land last, must be one of the actually-attempted values --
// not a corrupted mix, not empty/invalid.
func TestConcurrentSetStrategyGroupAndStatus(t *testing.T) {
	db := filepath.Join(t.TempDir(), "concurrent-mutations.db")
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	st, err := s.CreateStrategy("probe", "concurrent group/status mutation")
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}

	groups := []string{"g1", "g2", "g3", "g4"}
	statuses := []string{StrategyPlanning, StrategyActive, StrategyObserving}

	var wg sync.WaitGroup
	const rounds = 30
	errCh := make(chan error, rounds*2)
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		g := groups[i%len(groups)]
		st2 := statuses[i%len(statuses)]
		go func(g string) {
			defer wg.Done()
			if err := s.SetStrategyGroup(st.ID, g); err != nil {
				errCh <- fmt.Errorf("SetStrategyGroup(%s): %w", g, err)
			}
		}(g)
		go func(status string) {
			defer wg.Done()
			if err := s.SetStrategyStatus(st.ID, status); err != nil {
				errCh <- fmt.Errorf("SetStrategyStatus(%s): %w", status, err)
			}
		}(st2)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent mutation error: %v", err)
	}

	got, err := s.GetStrategy(st.ID)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	validGroup := false
	for _, g := range groups {
		if got.Group == g {
			validGroup = true
		}
	}
	if !validGroup {
		t.Fatalf("final Group %q is not one of the attempted values %v -- looks corrupted", got.Group, groups)
	}
	if !validStrategyStatuses[got.Status] {
		t.Fatalf("final Status %q is not a valid status at all -- looks corrupted", got.Status)
	}
}
