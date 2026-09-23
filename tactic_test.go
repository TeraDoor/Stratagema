package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// captureOutput redirects os.Stdout and os.Stderr for the duration of fn,
// returning everything written to either (mirroring cmd.CombinedOutput()
// in e2e_test.go, but for direct in-process calls rather than a
// subprocess) alongside fn's own return value.
func captureOutput(t *testing.T, fn func() int) (string, int) {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout, os.Stderr = w, w

	code := fn()

	w.Close()
	os.Stdout, os.Stderr = origOut, origErr

	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, rerr := r.Read(b)
		if n > 0 {
			buf.Write(b[:n])
		}
		if rerr != nil {
			break
		}
	}
	return buf.String(), code
}

// ── ParseTactic: real examples ─────────────────────────────────────────

// TestParseTacticRealExamples parses the two real, hand-written Tactic
// files this project's own vocabulary was defined against (copied
// byte-for-byte into testdata/ from docs/examples/stratagema-proto-strategy-{2,3}
// in the boat repo, not invented for this test) and asserts the exact
// fields a hand author actually wrote.
func TestParseTacticRealExamples(t *testing.T) {
	tests := []struct {
		file string
		want Tactic
	}{
		{
			file: "testdata/builder.md",
			want: Tactic{
				Name:       "builder",
				Capability: "go-development",
				Harness:    "claude-code",
				Tools:      []string{"read_file", "write_file", "exec"},
				Body: "Build one real, complete, tested package of a typical Go project against\n" +
					"a fixed interface contract declared in the Context — not an interface you\n" +
					"invent, since other Tactics' code depends on it existing exactly as\n" +
					"specified. Airtight means: table-driven tests, every documented error\n" +
					"path actually tested, `go vet` clean, `go test -race` clean, and edge\n" +
					"cases resolved and tested, not left ambiguous.\n" +
					"\n" +
					"Coordinate through Stratagema like any other engineer sharing this\n" +
					"codebase: claim your area with a lock before writing, using the\n" +
					"resource-naming convention its own docs recommend (repo-relative path),\n" +
					"and leave a release note precise enough that a Tactic who never talks to\n" +
					"you can build correctly against what you did.",
			},
		},
		{
			file: "testdata/observer.md",
			want: Tactic{
				Name:       "observer",
				Capability: "verification",
				Harness:    "claude-code",
				Tools:      []string{"read_file", "exec", "query_db"},
				Body: "A new role this project hasn't used before. Your job is specifically the\n" +
					"\"observe\" third of a strategy's lifecycle — not building anything, not\n" +
					"approving anything, just establishing what actually, verifiably happened.\n" +
					"\n" +
					"Two workers will each report their own account of what happened. Your job\n" +
					"is to check their accounts against the real, durable evidence — the\n" +
					"database's actual lock_events and propagations tables, not their\n" +
					"self-reports — and say plainly where the accounts match the evidence and\n" +
					"where they don't. A worker's account is a claim; the database is what\n" +
					"actually happened. Report both, and the gap between them if there is one.",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("reading %s: %v", tc.file, err)
			}
			got, err := ParseTactic(data)
			if err != nil {
				t.Fatalf("ParseTactic(%s): %v", tc.file, err)
			}
			if got.Name != tc.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.want.Name)
			}
			if got.Capability != tc.want.Capability {
				t.Errorf("Capability = %q, want %q", got.Capability, tc.want.Capability)
			}
			if got.Harness != tc.want.Harness {
				t.Errorf("Harness = %q, want %q", got.Harness, tc.want.Harness)
			}
			if len(got.Tools) != len(tc.want.Tools) {
				t.Fatalf("Tools = %v, want %v", got.Tools, tc.want.Tools)
			}
			for i := range got.Tools {
				if got.Tools[i] != tc.want.Tools[i] {
					t.Errorf("Tools[%d] = %q, want %q", i, got.Tools[i], tc.want.Tools[i])
				}
			}
			if got.Body != tc.want.Body {
				t.Errorf("Body mismatch:\ngot:  %q\nwant: %q", got.Body, tc.want.Body)
			}
		})
	}
}

// ── ParseTactic: core field, cut S085 ──────────────────────────────────

// TestParseTacticIgnoresLeftoverCoreLine documents a deliberate reversal,
// not a regression: the domain-neutral lifecycle-stage `core` field
// (plan|produce|verify|deliver) was added S077, found to have zero real
// consumers anywhere in this codebase (write+display only, never read or
// filtered on by anything — the same tactic that got external_signal cut
// S081), and cut S085. Unlike external_signal (a strategy_event kind,
// where an unknown kind is a hard parse error), a Tactic frontmatter key
// this parser doesn't recognize is silently ignored by design (see
// ParseTactic's own `default:` case) — so a hand-authored file with a
// leftover "core: produce" line from before this cut must still parse
// cleanly, just without the field doing anything, not error out.
func TestParseTacticIgnoresLeftoverCoreLine(t *testing.T) {
	input := "---\n" +
		"name: builder\n" +
		"capability: go-development\n" +
		"harness: claude-code\n" +
		"tools: [read_file]\n" +
		"core: produce\n" +
		"---\n" +
		"body\n"
	got, err := ParseTactic([]byte(input))
	if err != nil {
		t.Fatalf("ParseTactic with a leftover core: line: want it silently ignored, got error: %v", err)
	}
	if got.Name != "builder" || got.Body != "body" {
		t.Fatalf("ParseTactic with a leftover core: line: rest of the file didn't parse correctly, got %+v", got)
	}
}

// TestTacticRenderNeverEmitsCore confirms render() has no code path left
// that could write a "core:" line — cut cleanly, not just unreachable.
func TestTacticRenderNeverEmitsCore(t *testing.T) {
	f := &Tactic{
		Name:       "builder",
		Capability: "go-development",
		Harness:    "claude-code",
		Tools:      []string{"read_file"},
		Body:       "body",
	}
	if strings.Contains(f.render(), "core:") {
		t.Fatalf("render() contains a \"core:\" line, want it never emitted:\n%s", f.render())
	}
}

// ── ParseTactic: malformed input ───────────────────────────────────────

func TestParseTacticMalformed(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error // checked with errors.Is when non-nil
		wantSub string // substring checked when wantErr is nil
	}{
		{
			name: "no opening delimiter",
			input: "name: builder\n" +
				"capability: go-development\n" +
				"harness: claude-code\n" +
				"tools: [read_file]\n" +
				"---\n" +
				"body\n",
			wantErr: ErrMissingOpeningDelimiter,
		},
		{
			name: "no closing delimiter",
			input: "---\n" +
				"name: builder\n" +
				"capability: go-development\n" +
				"harness: claude-code\n" +
				"tools: [read_file]\n" +
				"body, never closed\n",
			wantErr: ErrMissingClosingDelimiter,
		},
		{
			name: "empty tools list",
			input: "---\n" +
				"name: builder\n" +
				"capability: go-development\n" +
				"harness: claude-code\n" +
				"tools: []\n" +
				"---\n" +
				"body\n",
			wantErr: ErrToolsEmpty,
		},
		{
			name: "tools value not a bracketed list",
			input: "---\n" +
				"name: builder\n" +
				"capability: go-development\n" +
				"harness: claude-code\n" +
				"tools: read_file, write_file\n" +
				"---\n" +
				"body\n",
			wantErr: ErrToolsNotBracketList,
		},
		{
			name: "missing name field",
			input: "---\n" +
				"capability: go-development\n" +
				"harness: claude-code\n" +
				"tools: [read_file]\n" +
				"---\n" +
				"body\n",
			wantSub: `missing required frontmatter field "name"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTactic([]byte(tc.input))
			if err == nil {
				t.Fatalf("ParseTactic(%q): want error, got nil", tc.name)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseTactic(%q): got error %v, want it to wrap %v", tc.name, err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("ParseTactic(%q): error %q does not contain %q", tc.name, err.Error(), tc.wantSub)
			}
		})
	}
}

// ── CLI ──────────────────────────────────────────────────────────────────

func TestTacticCreateListShow(t *testing.T) {
	dir := t.TempDir()

	out, code := captureOutput(t, func() int {
		return cmdTacticCreate([]string{
			"-tactics-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file,write_file,exec",
		})
	})
	if code != 0 {
		t.Fatalf("tactic create: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "created") || !strings.Contains(out, "builder.md") {
		t.Fatalf("tactic create: unexpected output:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdTacticList([]string{"-tactics-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("tactic list: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "builder") || !strings.Contains(out, "go-development") || !strings.Contains(out, "claude-code") {
		t.Fatalf("tactic list: want builder/go-development/claude-code in output, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdTacticShow([]string{"-tactics-dir=" + dir, "builder"})
	})
	if code != 0 {
		t.Fatalf("tactic show: exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"name:       builder", "capability: go-development", "harness:    claude-code", "read_file, write_file, exec"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tactic show: want %q in output, got:\n%s", want, out)
		}
	}
}

// TestTacticCreateRejectsCoreFlag confirms -core is no longer a
// recognized flag on `tactic create` at all (cut S085, not just left
// optional) — flag.ExitOnError means passing an unknown flag exits the
// process directly, so this is checked via a real subprocess rather than
// calling cmdTacticCreate in-process (which would kill the test binary).
func TestTacticCreateRejectsCoreFlag(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cmd := exec.Command(bin, "tactic", "create",
		"-tactics-dir="+dir, "-name=leader", "-capability=orchestration",
		"-harness=claude-code", "-tools=read_file", "-core=plan")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("tactic create -core=plan: want a real error (flag no longer exists), got exit 0:\n%s", out)
	}
	if !strings.Contains(string(out), "flag provided but not defined") {
		t.Fatalf("tactic create -core=plan: want an unknown-flag error, got:\n%s", out)
	}
}

func TestTacticCreateRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	create := func() (string, int) {
		return captureOutput(t, func() int {
			return cmdTacticCreate([]string{
				"-tactics-dir=" + dir,
				"-name=builder",
				"-capability=go-development",
				"-harness=claude-code",
				"-tools=read_file",
			})
		})
	}

	if _, code := create(); code != 0 {
		t.Fatalf("first create: want success")
	}
	path := filepath.Join(dir, "builder.md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading created file: %v", err)
	}

	out, code := create()
	if code == 0 {
		t.Fatalf("second create: want non-zero exit (refuse overwrite), got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Fatalf("second create: want a real \"already exists\" error, got:\n%s", out)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file after refused overwrite: %v", err)
	}
	if string(after) != string(original) {
		t.Fatalf("refused overwrite still modified the file on disk")
	}
}

// TestTacticListIncludesHandwrittenFile is the automated counterpart of
// this project's own MANUAL_TEST.md pending item: a directory containing
// one Tactic created via the CLI and one authored entirely by hand (the
// real observer.md fixture, not something this test invents) must list
// both correctly — hand-authored Tactics are the expected common case,
// not an edge case.
func TestTacticListIncludesHandwrittenFile(t *testing.T) {
	dir := t.TempDir()

	_, code := captureOutput(t, func() int {
		return cmdTacticCreate([]string{
			"-tactics-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file,write_file,exec",
		})
	})
	if code != 0 {
		t.Fatalf("tactic create: want success")
	}

	handwritten, err := os.ReadFile("testdata/observer.md")
	if err != nil {
		t.Fatalf("reading testdata/observer.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observer.md"), handwritten, 0o644); err != nil {
		t.Fatalf("writing hand-authored fixture: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdTacticList([]string{"-tactics-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("tactic list: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("tactic list: missing CLI-created builder, got:\n%s", out)
	}
	if !strings.Contains(out, "observer") || !strings.Contains(out, "verification") {
		t.Fatalf("tactic list: missing hand-authored observer, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdTacticShow([]string{"-tactics-dir=" + dir, "observer"})
	})
	if code != 0 {
		t.Fatalf("tactic show observer: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "query_db") {
		t.Fatalf("tactic show observer: want its real tools in output, got:\n%s", out)
	}
}

// TestTacticListReportsUnparseableFileWithoutHidingOthers confirms a
// broken hand-edited file is reported clearly by name, and does not take
// down the whole listing or hide a sibling tactic that's fine.
func TestTacticListReportsUnparseableFileWithoutHidingOthers(t *testing.T) {
	dir := t.TempDir()

	_, code := captureOutput(t, func() int {
		return cmdTacticCreate([]string{
			"-tactics-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file",
		})
	})
	if code != 0 {
		t.Fatalf("tactic create: want success")
	}

	broken := filepath.Join(dir, "broken.md")
	if err := os.WriteFile(broken, []byte("not a tactic file at all\n"), 0o644); err != nil {
		t.Fatalf("writing broken.md: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdTacticList([]string{"-tactics-dir=" + dir})
	})
	if code == 0 {
		t.Fatalf("tactic list: want non-zero exit when a file fails to parse")
	}
	if !strings.Contains(out, "broken.md") {
		t.Fatalf("tactic list: want the broken file named in output, got:\n%s", out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("tactic list: a broken file should not hide a valid sibling, got:\n%s", out)
	}
}

func TestTacticDirResolutionMirrorsDBPath(t *testing.T) {
	t.Setenv("STRATAGEMA_TACTICS", "")
	if got, want := resolveTacticsDir(""), filepath.Join(".stratagema", "tactics"); got != want {
		t.Fatalf("resolveTacticsDir(\"\") with no env = %q, want %q", got, want)
	}

	t.Setenv("STRATAGEMA_TACTICS", "/tmp/custom-tactics")
	if got, want := resolveTacticsDir(""), "/tmp/custom-tactics"; got != want {
		t.Fatalf("resolveTacticsDir(\"\") with env set = %q, want %q", got, want)
	}

	if got, want := resolveTacticsDir("/explicit/flag/dir"), "/explicit/flag/dir"; got != want {
		t.Fatalf("resolveTacticsDir(explicit) = %q, want %q (flag should win over env)", got, want)
	}
}
