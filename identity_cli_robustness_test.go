package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the "everything except VerifyIdentityToken/-protect/token
// auth" pass over identity.go, plus a dedicated pass over cli.go's shared
// flag helpers and main.go's top-level command dispatch -- neither of which
// had a dedicated test file before. identity_auth_test.go already covers
// the token/protect/auth-gate surface in depth; nothing here duplicates it.

// ── label field-type edge cases (Store level) ──────────────────────────────

// TestCreateIdentityLabelEmptyAllowedAtStoreLevel confirms where the
// "-label is required" validation actually lives: it's a CLI-layer check
// in cmdIdentityCreate (identity.go), not a Store-layer invariant. The Store
// itself places no constraint on label content, matching the doc comment's
// "deliberately just a label" framing -- worth confirming directly rather
// than assuming the CLI's check is backed by a DB constraint too.
func TestCreateIdentityLabelEmptyAllowedAtStoreLevel(t *testing.T) {
	s := newTestStore(t)
	it, err := s.CreateIdentity("")
	if err != nil {
		t.Fatalf("CreateIdentity(\"\") at Store level: %v, want success (only the CLI layer rejects an empty label)", err)
	}
	if it.Label != "" {
		t.Fatalf("Label = %q, want empty string preserved exactly", it.Label)
	}
}

// TestCreateIdentityLabelUnicodeAndLongRoundTrip proves a label round-trips
// through SQLite exactly, at both a long length (100KB) and with non-ASCII
// content (CJK + an emoji outside the BMP), with nothing truncated, mangled,
// or mis-decoded.
func TestCreateIdentityLabelUnicodeAndLongRoundTrip(t *testing.T) {
	s := newTestStore(t)

	long := strings.Repeat("A", 100_000)
	itLong, err := s.CreateIdentity(long)
	if err != nil {
		t.Fatalf("CreateIdentity(100KB label): %v", err)
	}
	if itLong.Label != long {
		t.Fatalf("long label round-trip: got length %d, want %d", len(itLong.Label), len(long))
	}

	unicode := "日本語 emoji 🎉 test — em-dash too"
	itUni, err := s.CreateIdentity(unicode)
	if err != nil {
		t.Fatalf("CreateIdentity(unicode label): %v", err)
	}
	if itUni.Label != unicode {
		t.Fatalf("unicode label round-trip: got %q, want %q", itUni.Label, unicode)
	}

	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListIdentities: got %d, want 2", len(all))
	}
	for _, it := range all {
		if it.ID == itLong.ID && it.Label != long {
			t.Fatalf("ListIdentities lost/mangled the long label")
		}
		if it.ID == itUni.ID && it.Label != unicode {
			t.Fatalf("ListIdentities lost/mangled the unicode label, got %q", it.Label)
		}
	}
}

// TestCreateIdentityDuplicateLabelsAllowed confirms a real design question
// by checking the code rather than assuming an answer either way: the
// identities table (store.go's migrate()) has no UNIQUE constraint on
// label, and createIdentity never checks for an existing row with the same
// label. Two identities with the identical label are therefore allowed and
// remain distinct rows with distinct IDs -- consistent with the doc
// comment's framing of label as display text, not an identifier (IDs, never
// labels, are what VerifyIdentityToken/AcquireLock/etc. key off of).
func TestCreateIdentityDuplicateLabelsAllowed(t *testing.T) {
	s := newTestStore(t)
	a, err := s.CreateIdentity("agent-x")
	if err != nil {
		t.Fatalf("CreateIdentity #1: %v", err)
	}
	b, err := s.CreateIdentity("agent-x")
	if err != nil {
		t.Fatalf("CreateIdentity #2 (duplicate label): %v, want success -- duplicate labels are not rejected by this schema", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two CreateIdentity calls produced the same ID %s", a.ID)
	}
	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListIdentities: got %d rows, want 2 distinct identities sharing one label", len(all))
	}
}

// TestCreateIdentityLabelSQLMetacharactersSafe proves the insert is
// parameterized, not string-built: a label containing quote/semicolon/SQL
// keyword characters that would be dangerous in a concatenated query is
// stored and returned completely verbatim, and does not affect any other
// row.
func TestCreateIdentityLabelSQLMetacharactersSafe(t *testing.T) {
	s := newTestStore(t)
	evil := `'; DROP TABLE identities; --` + "\" OR \"1\"=\"1"
	victim, err := s.CreateIdentity("victim")
	if err != nil {
		t.Fatalf("CreateIdentity(victim): %v", err)
	}
	attacker, err := s.CreateIdentity(evil)
	if err != nil {
		t.Fatalf("CreateIdentity(sql-metacharacter label): %v", err)
	}
	if attacker.Label != evil {
		t.Fatalf("label round-trip: got %q, want %q (not parameterized correctly?)", attacker.Label, evil)
	}
	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities after supposed injection attempt: %v (table should be untouched)", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListIdentities: got %d rows, want 2 -- an injection would likely have dropped the table or altered row count", len(all))
	}
	found := false
	for _, it := range all {
		if it.ID == victim.ID {
			found = true
			if it.Label != "victim" {
				t.Fatalf("victim row's label was altered to %q", it.Label)
			}
		}
	}
	if !found {
		t.Fatalf("victim identity %s vanished -- table was affected by the other row's label content", victim.ID)
	}
}

// ── identity list / create display: newline-safety (real bug found + fixed) ─

// TestIdentityListEscapesNewlineInLabel is the identity.go instance of the
// exact display bug already found and fixed for strategy.go (see
// escapeForSingleLineDisplay's doc comment and
// strategy_interest_robustness_test.go): before the fix in this session,
// identity list printed a label's raw embedded newline directly, splitting
// one identity's row across multiple visual lines and breaking the
// one-row-per-identity contract every reader of this output relies on.
// identity.go was explicitly not part of the original fix, so it had this
// exposure independently.
func TestIdentityListEscapesNewlineInLabel(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateIdentity("agent-before"); err != nil {
		t.Fatalf("CreateIdentity(agent-before): %v", err)
	}
	if _, err := s.CreateIdentity("evil\nlabel\r\nwith\rbreaks"); err != nil {
		t.Fatalf("CreateIdentity(newline label): %v", err)
	}
	if _, err := s.CreateIdentity("agent-after"); err != nil {
		t.Fatalf("CreateIdentity(agent-after): %v", err)
	}

	db := storeDBPath(t, s)
	out, code := captureOutput(t, func() int {
		return cmdIdentityList([]string{"-db=" + db})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityList: exit %d, output:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("identity list: want exactly 3 lines (one per identity), got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `evil\nlabel\nwith\nbreaks`) {
		t.Fatalf("identity list: want the newline label escaped as literal \\n, got:\n%s", out)
	}
	if strings.Contains(out, "\r") {
		t.Fatalf("identity list: a raw \\r made it into the output unescaped:\n%s", out)
	}
}

// TestIdentityCreateConfirmationEscapesNewlineInLabel covers the other
// print site identity.go has for a label: the "created ..." confirmation
// cmdIdentityCreate prints immediately after creation (both the -protect
// and non-protect paths use the same escapeForSingleLineDisplay call).
func TestIdentityCreateConfirmationEscapesNewlineInLabel(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + db, "-label=line1\nline2"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate: exit %d, output:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("cmdIdentityCreate confirmation: want exactly 2 lines (\"created ...\" + \"  label: ...\"), got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `label: line1\nline2`) {
		t.Fatalf("cmdIdentityCreate: want the label's newline escaped, got:\n%s", out)
	}
}

// TestIdentityListAtScale creates 150 identities and confirms `identity
// list` renders exactly one line per identity, still ordered by creation
// time (ListIdentities' own ORDER BY created_at), and returns in a
// reasonable time -- a loose sanity bound, not a tight benchmark.
func TestIdentityListAtScale(t *testing.T) {
	s := newTestStore(t)
	const n = 150
	var labels []string
	for i := 0; i < n; i++ {
		label := fmt.Sprintf("agent-%03d", i)
		labels = append(labels, label)
		if _, err := s.CreateIdentity(label); err != nil {
			t.Fatalf("CreateIdentity(%s): %v", label, err)
		}
	}

	db := storeDBPath(t, s)
	start := time.Now()
	out, code := captureOutput(t, func() int {
		return cmdIdentityList([]string{"-db=" + db})
	})
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("cmdIdentityList: exit %d", code)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cmdIdentityList over %d identities took %s, want well under 5s", n, elapsed)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("identity list: want %d lines, got %d", n, len(lines))
	}
	for i, label := range labels {
		if !strings.Contains(lines[i], label) {
			t.Fatalf("line %d: want label %q (creation order), got %q", i, label, lines[i])
		}
	}
}

// TestIdentityListAdversarialLabelsAtScale mixes newline-containing, tab-
// containing, and unicode labels into a moderately large set and confirms
// the row count survives exactly -- the same class of check as
// TestIdentityListAtScale, but proving the escaping fix holds under volume,
// not just for one isolated identity.
func TestIdentityListAdversarialLabelsAtScale(t *testing.T) {
	s := newTestStore(t)
	adversarial := []string{
		"plain",
		"has\nnewline",
		"has\r\ncrlf",
		"has\rbare-cr",
		"has\ttab",
		"日本語ラベル",
		"emoji🎉label",
		strings.Repeat("x\n", 20),
	}
	const repeats = 20
	want := 0
	for i := 0; i < repeats; i++ {
		for _, l := range adversarial {
			if _, err := s.CreateIdentity(l); err != nil {
				t.Fatalf("CreateIdentity(%q): %v", l, err)
			}
			want++
		}
	}

	db := storeDBPath(t, s)
	out, code := captureOutput(t, func() int {
		return cmdIdentityList([]string{"-db=" + db})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityList: exit %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != want {
		t.Fatalf("identity list: want %d lines (one per identity, all newlines escaped), got %d", want, len(lines))
	}
}

// storeDBPath recovers the on-disk path newTestStore's *Store was opened
// with, so a test can reuse the same file across a Store-level setup phase
// and a cmd-level (CLI) assertion phase without threading the path through
// separately. sqlite's DB_LIST pragma reports it back exactly.
func storeDBPath(t *testing.T, s *Store) string {
	t.Helper()
	row := s.db.QueryRow(`PRAGMA database_list`)
	var seq int
	var name, path string
	if err := row.Scan(&seq, &name, &path); err != nil {
		t.Fatalf("PRAGMA database_list: %v", err)
	}
	if path == "" {
		t.Fatalf("PRAGMA database_list returned an empty path")
	}
	return path
}

// ── top-level CLI argument dispatch (main.go, cli.go) ──────────────────────

// TestCLINoArguments proves main.go's `len(os.Args) < 2` branch: no
// arguments at all prints usage and exits 2, not a panic or silent no-op.
func TestCLINoArguments(t *testing.T) {
	bin := buildBinary(t)
	out, err := exec.Command(bin).CombinedOutput()
	code := exitCode(t, err)
	if code != 2 {
		t.Fatalf("no args: exit %d, want 2, output:\n%s", code, out)
	}
	if !strings.Contains(string(out), "usage: stratagema") {
		t.Fatalf("no args: want usage text, got:\n%s", out)
	}
}

// TestCLIUnknownTopLevelCommand proves the default case of main.go's
// switch: an unrecognized command names itself in the error, still prints
// usage, and exits 2.
func TestCLIUnknownTopLevelCommand(t *testing.T) {
	bin := buildBinary(t)
	out, err := exec.Command(bin, "totally-bogus-command").CombinedOutput()
	code := exitCode(t, err)
	if code != 2 {
		t.Fatalf("unknown command: exit %d, want 2, output:\n%s", code, out)
	}
	if !strings.Contains(string(out), `unknown command "totally-bogus-command"`) {
		t.Fatalf("unknown command: want the bad command named in the error, got:\n%s", out)
	}
}

// TestTopLevelHelpVariants proves all three forms main.go's switch wires
// to usage() -- "-h", "--help", "help" -- exit 0 (not 2, unlike every other
// unrecognized-input path) and print the same usage text.
func TestTopLevelHelpVariants(t *testing.T) {
	bin := buildBinary(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		out, err := exec.Command(bin, arg).CombinedOutput()
		code := exitCode(t, err)
		if code != 0 {
			t.Fatalf("%s: exit %d, want 0, output:\n%s", arg, code, out)
		}
		if !strings.Contains(string(out), "usage: stratagema") {
			t.Fatalf("%s: want usage text, got:\n%s", arg, out)
		}
	}
}

// TestIdentityNoSubcommandRequiresOne covers cmdIdentity's own guard
// (identity.go): `stratagema identity` with no subcommand prints its own
// scoped usage line and exits 2, distinct from main.go's top-level usage.
func TestIdentityNoSubcommandRequiresOne(t *testing.T) {
	out, code := captureOutput(t, func() int { return cmdIdentity(nil) })
	if code != 2 {
		t.Fatalf("cmdIdentity(nil): exit %d, want 2, output:\n%s", code, out)
	}
	if !strings.Contains(out, "usage: stratagema identity") {
		t.Fatalf("cmdIdentity(nil): want scoped usage text, got:\n%s", out)
	}
}

// TestIdentityUnknownSubcommand covers cmdIdentity's default case: an
// unrecognized subcommand is named in the error and exits 2, not silently
// ignored or panicking.
func TestIdentityUnknownSubcommand(t *testing.T) {
	out, code := captureOutput(t, func() int { return cmdIdentity([]string{"bogus"}) })
	if code != 2 {
		t.Fatalf("cmdIdentity([bogus]): exit %d, want 2, output:\n%s", code, out)
	}
	if !strings.Contains(out, `unknown subcommand "bogus"`) {
		t.Fatalf("cmdIdentity([bogus]): want the bad subcommand named, got:\n%s", out)
	}
}

// TestIdentityHelpNotSpecialCasedAtSubcommandLevel documents current,
// verified-intentional behavior rather than guessing: `stratagema identity
// -h` is NOT special-cased the way the top level is. "-h" falls into
// cmdIdentity's switch as an ordinary (unknown) subcommand and is rejected
// with the same error/exit code any other bad subcommand gets. This is not
// unique to identity -- lock, faculty, strategy, and interest all have the
// identical dispatcher shape (confirmed by reading each), so this is a
// consistent, if under-documented, codebase-wide convention, not an
// identity-specific bug. Changing it only here would make identity diverge
// from its four siblings, which is out of this pass's scope (identity.go /
// cli.go / main.go only) -- see the doc comment now on cmdIdentity itself.
// Per-leaf help (`identity create -h`) still works, proven separately below.
func TestIdentityHelpNotSpecialCasedAtSubcommandLevel(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		out, code := captureOutput(t, func() int { return cmdIdentity([]string{arg}) })
		if code != 2 {
			t.Fatalf("cmdIdentity([%s]): exit %d, want 2 (treated as an unknown subcommand, matching lock/faculty/strategy/interest)", arg, code)
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Fatalf("cmdIdentity([%s]): want an unknown-subcommand message, got:\n%s", arg, out)
		}
	}
}

// TestIdentityCreateAndListHelpViaFlagPackage proves per-leaf help *is*
// wired, just via the standard library's own mechanism rather than
// identity.go's switch: flag.ExitOnError intercepts "-h" before
// fs.Parse returns, prints the flag usage, and calls os.Exit(0) itself --
// which is why this must run as a real subprocess rather than an in-process
// captureOutput call (os.Exit there would kill the test binary).
func TestIdentityCreateAndListHelpViaFlagPackage(t *testing.T) {
	bin := buildBinary(t)
	cases := []struct {
		args      []string
		wantUsage string
	}{
		{[]string{"identity", "create", "-h"}, "Usage of identity create"},
		{[]string{"identity", "create", "--help"}, "Usage of identity create"},
		{[]string{"identity", "list", "-h"}, "Usage of identity list"},
	}
	for _, c := range cases {
		out, err := exec.Command(bin, c.args...).CombinedOutput()
		code := exitCode(t, err)
		if code != 0 {
			t.Fatalf("%v: exit %d, want 0, output:\n%s", c.args, code, out)
		}
		if !strings.Contains(string(out), c.wantUsage) {
			t.Fatalf("%v: want output containing %q, got:\n%s", c.args, c.wantUsage, out)
		}
	}
}

// exitCode extracts a subprocess's real exit code from exec.Command's
// error, the same pattern e2e_test.go uses elsewhere in this codebase,
// rather than treating any non-nil err as a generic failure.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	switch e := err.(type) {
	case nil:
		return 0
	case *exec.ExitError:
		return e.ExitCode()
	default:
		t.Fatalf("command failed to even start: %v", err)
		return -1
	}
}

// TestIdentityCreateDuplicateFlagLastWins proves Go's flag package's own,
// standard behavior applies here unmodified: when the same flag is given
// twice, the later value wins, not the first and not an error. Worth
// confirming directly for this specific flag rather than assuming the
// stdlib's documented behavior is what actually ships.
func TestIdentityCreateDuplicateFlagLastWins(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + db, "-label=first", "-label=second"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "label: second") {
		t.Fatalf("duplicate -label: want the later value (\"second\") to win, got:\n%s", out)
	}
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer s.Close()
	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 1 || all[0].Label != "second" {
		t.Fatalf("stored identity: want exactly 1 with label \"second\", got %+v", all)
	}
}

// TestIdentityCreateFlagValueLooksLikeAnotherFlag proves -label=-protect is
// parsed as the literal label string "-protect", not as a second,
// malformed attempt to set the -protect flag -- Go's flag package resolves
// "-label=-protect" as a single token (name=value), so this is expected
// stdlib behavior, confirmed rather than assumed.
func TestIdentityCreateFlagValueLooksLikeAnotherFlag(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + db, "-label=-protect"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "label: -protect") {
		t.Fatalf("want the literal label \"-protect\" preserved, got:\n%s", out)
	}
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer s.Close()
	all, err := s.ListIdentities()
	if err != nil {
		t.Fatalf("ListIdentities: %v", err)
	}
	if len(all) != 1 || all[0].Protected {
		t.Fatalf("want exactly 1 unprotected identity (the -protect flag was never actually set), got %+v", all)
	}
}

// TestIdentityCreateTrailingPositionalArgRejected is the real bug this pass
// found and fixed: identity create takes zero positional arguments, but
// before the fix, cmdIdentityCreate never checked fs.NArg(), so a stray
// argument was silently ignored -- and worse, per Go's documented flag.Parse
// behavior (it stops parsing at the first non-flag token, the exact gotcha
// strategy_e2e_test.go's own run() helper works around), any flag placed
// after that stray argument was silently dropped too. A user typing
// `identity create -label=x extra -protect` got an unprotected identity
// with no error, no warning -- silently not what they asked for. Now it's a
// clean, named error instead.
func TestIdentityCreateTrailingPositionalArgRejected(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + db, "-label=x", "extra-arg", "-protect"})
	})
	if code == 0 {
		t.Fatalf("cmdIdentityCreate with a trailing positional arg before -protect: want a non-zero exit (this used to silently drop -protect), got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "unexpected argument") {
		t.Fatalf("want a clear \"unexpected argument\" error, got:\n%s", out)
	}
	// And confirm nothing was created at all -- the command must fail
	// before ever touching the store, not create an unprotected identity
	// and then report the error.
	if _, err := os.Stat(db); err == nil {
		t.Fatalf("db file %s was created despite the command being rejected", db)
	}
}

// TestIdentityListTrailingPositionalArgRejected is the identity-list half
// of the same fix. Before it, `identity list extra -db=foo.db` silently
// dropped the -db flag entirely (it came after the stray "extra" token) and
// fell back to the default db path with no indication anything was
// ignored -- a query that looked like it was answering "what's in foo.db"
// was actually silently answering "what's in the default db instead."
func TestIdentityListTrailingPositionalArgRejected(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := openLocalStore(db)
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	if _, err := s.CreateIdentity("should-not-be-silently-hidden"); err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	s.Close()

	out, code := captureOutput(t, func() int {
		return cmdIdentityList([]string{"extra-arg", "-db=" + db})
	})
	if code == 0 {
		t.Fatalf("cmdIdentityList with a trailing positional arg before -db: want a non-zero exit (this used to silently ignore -db and query the default path instead), got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "unexpected argument") {
		t.Fatalf("want a clear \"unexpected argument\" error, got:\n%s", out)
	}
	if strings.Contains(out, "no identities") {
		t.Fatalf("cmdIdentityList silently fell back to the default (empty) db instead of erroring:\n%s", out)
	}
}

// TestIdentityCreateVeryLongArgumentList stresses the argument-count axis
// directly rather than just argument content: several thousand repeated
// flags, ending in the one that actually matters, still parses correctly
// and finishes quickly (Go's flag package has no documented argument-count
// limit, but this project's own handling of a long argv is otherwise
// untested).
func TestIdentityCreateVeryLongArgumentList(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	args := []string{"-db=" + db}
	for i := 0; i < 5000; i++ {
		args = append(args, fmt.Sprintf("-label=decoy-%d", i))
	}
	args = append(args, "-label=final")

	start := time.Now()
	out, code := captureOutput(t, func() int { return cmdIdentityCreate(args) })
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("cmdIdentityCreate with 5001 -label flags: exit %d, output:\n%s", code, out)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cmdIdentityCreate with a long argument list took %s, want well under 5s", elapsed)
	}
	if !strings.Contains(out, "label: final") {
		t.Fatalf("want the final -label value (\"final\", last one wins) to be used, got:\n%s", out)
	}
}

// TestNullByteInArgNeverReachesTheProgram documents, with real evidence
// rather than assumption, why a null byte in an argument isn't a case
// identity.go's own argument parsing needs to defend against: exec.Command
// (used here directly, bypassing any shell) fails at the OS/exec layer
// before the target process is even started, because a null byte cannot
// appear in a NUL-terminated C-string argv entry, which is what every OS
// exec syscall requires. The program never sees this input, on any
// platform Go supports.
func TestNullByteInArgNeverReachesTheProgram(t *testing.T) {
	bin := buildBinary(t)
	db := filepath.Join(t.TempDir(), "test.db")
	cmd := exec.Command(bin, "identity", "create", "-db="+db, "-label=has\x00null")
	if err := cmd.Run(); err == nil {
		t.Fatalf("exec.Command with a NUL byte in an argument: want a start-up error from the OS/exec layer, got none")
	}
}

// ── -db / dbPathFlag path resolution edge cases ─────────────────────────────

// TestDBFlagOverridesEnvVar confirms dbPathFlag/openStore's documented
// precedence (cli.go's own flag help text: "default: $STRATAGEMA_DB, else
// ./.stratagema/events.db") the same way resolveToken's precedence is
// proven in identity_auth_test.go: an explicit -db flag wins over
// STRATAGEMA_DB, not the other way around, and the env-only path is never
// even created.
func TestDBFlagOverridesEnvVar(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env.db")
	flagPath := filepath.Join(dir, "flag.db")

	orig, had := os.LookupEnv("STRATAGEMA_DB")
	os.Setenv("STRATAGEMA_DB", envPath)
	t.Cleanup(func() {
		if had {
			os.Setenv("STRATAGEMA_DB", orig)
		} else {
			os.Unsetenv("STRATAGEMA_DB")
		}
	})

	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + flagPath, "-label=flag-should-win"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate: exit %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(flagPath); err != nil {
		t.Fatalf("-db path %s was not created: %v", flagPath, err)
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Fatalf("STRATAGEMA_DB path %s was created even though -db was also given -- flag did not win", envPath)
	}
}

// TestDBPathIsDirectoryErrorsCleanly gives -db a real directory (not a
// file) and confirms the command fails with die's clean, uniform error
// path, not a panic or an unhandled low-level error dump.
func TestDBPathIsDirectoryErrorsCleanly(t *testing.T) {
	dir := t.TempDir()
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + dir, "-label=x"})
	})
	if code == 0 {
		t.Fatalf("cmdIdentityCreate with -db pointing at a directory: want a non-zero exit, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "stratagema: identity create:") {
		t.Fatalf("want the uniform die() error prefix, got:\n%s", out)
	}
}

// TestDBPathUnwritableDirectoryErrorsCleanly uses a real os.Chmod (not a
// simulated permission check) to make a parent directory unwritable, then
// confirms opening a db path underneath it fails cleanly via die rather
// than panicking. Skipped when running as root, where Unix permission bits
// don't restrict access the same way.
func TestDBPathUnwritableDirectoryErrorsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't restrict root, this test would be meaningless")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod(0o000): %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // restore so t.TempDir() cleanup can remove it

	dbPath := filepath.Join(locked, "sub", "test.db")
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + dbPath, "-label=x"})
	})
	if code == 0 {
		t.Fatalf("cmdIdentityCreate under an unwritable directory: want a non-zero exit, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "stratagema: identity create:") {
		t.Fatalf("want the uniform die() error prefix, got:\n%s", out)
	}
}

// TestDBPathRelativeVsAbsoluteFromDifferentWorkingDirectories confirms both
// a relative and an absolute -db path resolve to the same file regardless
// of the process's current working directory -- relative to cwd (the
// standard os/filepath contract openLocalStore relies on with no special
// handling of its own), absolute unconditionally.
func TestDBPathRelativeVsAbsoluteFromDifferentWorkingDirectories(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })

	// Relative path, run from "sub": resolves relative to "sub", not root.
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("Chdir(sub): %v", err)
	}
	out, code := captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=rel.db", "-label=relative"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate (relative -db): exit %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(sub, "rel.db")); err != nil {
		t.Fatalf("relative -db=rel.db from cwd %s did not create %s/rel.db: %v", sub, sub, err)
	}
	if _, err := os.Stat(filepath.Join(root, "rel.db")); err == nil {
		t.Fatalf("relative -db=rel.db unexpectedly resolved against root instead of cwd (%s)", sub)
	}

	// Absolute path, run from "sub" again: resolves to the same file
	// regardless of a further cwd change back to root.
	absPath := filepath.Join(root, "abs.db")
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(root): %v", err)
	}
	out, code = captureOutput(t, func() int {
		return cmdIdentityCreate([]string{"-db=" + absPath, "-label=absolute"})
	})
	if code != 0 {
		t.Fatalf("cmdIdentityCreate (absolute -db): exit %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(absPath); err != nil {
		t.Fatalf("absolute -db=%s did not create the file: %v", absPath, err)
	}
}
