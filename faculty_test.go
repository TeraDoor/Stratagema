package main

import (
	"errors"
	"os"
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

// ── ParseFaculty: real examples ─────────────────────────────────────────

// TestParseFacultyRealExamples parses the two real, hand-written Faculty
// files this project's own vocabulary was defined against (copied
// byte-for-byte into testdata/ from docs/examples/stratagema-proto-strategy-{2,3}
// in the boat repo, not invented for this test) and asserts the exact
// fields a hand author actually wrote.
func TestParseFacultyRealExamples(t *testing.T) {
	tests := []struct {
		file string
		want Faculty
	}{
		{
			file: "testdata/builder.md",
			want: Faculty{
				Name:       "builder",
				Capability: "go-development",
				Harness:    "claude-code",
				Tools:      []string{"read_file", "write_file", "exec"},
				Body: "Build one real, complete, tested package of a typical Go project against\n" +
					"a fixed interface contract declared in the Context — not an interface you\n" +
					"invent, since other Faculties' code depends on it existing exactly as\n" +
					"specified. Airtight means: table-driven tests, every documented error\n" +
					"path actually tested, `go vet` clean, `go test -race` clean, and edge\n" +
					"cases resolved and tested, not left ambiguous.\n" +
					"\n" +
					"Coordinate through Stratagema like any other engineer sharing this\n" +
					"codebase: claim your area with a lock before writing, using the\n" +
					"resource-naming convention its own docs recommend (repo-relative path),\n" +
					"and leave a release note precise enough that a Faculty who never talks to\n" +
					"you can build correctly against what you did.",
			},
		},
		{
			file: "testdata/observer.md",
			want: Faculty{
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
			got, err := ParseFaculty(data)
			if err != nil {
				t.Fatalf("ParseFaculty(%s): %v", tc.file, err)
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

// ── ParseFaculty: core field ────────────────────────────────────────────

// TestParseFacultyCoreOptional confirms the two real fixtures parsed above
// — written before `core` existed — still parse cleanly with Core == "".
// This is the backward-compatibility guarantee the field was added under:
// a frontmatter key absent from a file predates this change entirely, not
// just an unset optional value.
func TestParseFacultyCoreOptional(t *testing.T) {
	for _, file := range []string{"testdata/builder.md", "testdata/observer.md"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		got, err := ParseFaculty(data)
		if err != nil {
			t.Fatalf("ParseFaculty(%s): %v", file, err)
		}
		if got.Core != "" {
			t.Errorf("ParseFaculty(%s): Core = %q, want empty (file predates the core field)", file, got.Core)
		}
	}
}

// TestParseFacultyCoreValidValues confirms each of the four domain-neutral
// lifecycle stages parses and round-trips through render.
func TestParseFacultyCoreValidValues(t *testing.T) {
	for _, core := range []string{"plan", "produce", "verify", "deliver"} {
		input := "---\n" +
			"name: builder\n" +
			"capability: go-development\n" +
			"harness: claude-code\n" +
			"tools: [read_file]\n" +
			"core: " + core + "\n" +
			"---\n" +
			"body\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("ParseFaculty(core=%s): %v", core, err)
		}
		if got.Core != core {
			t.Errorf("ParseFaculty(core=%s): Core = %q, want %q", core, got.Core, core)
		}

		rendered := got.render()
		reparsed, err := ParseFaculty([]byte(rendered))
		if err != nil {
			t.Fatalf("re-parsing rendered output for core=%s: %v", core, err)
		}
		if reparsed.Core != core {
			t.Errorf("round-trip core=%s: got %q after render+reparse", core, reparsed.Core)
		}
	}
}

// TestParseFacultyCoreInvalidValue confirms a core value outside the four
// allowed stages is a real, specific parse error — not silently ignored
// (which would hide an author's typo) and not a generic failure.
func TestParseFacultyCoreInvalidValue(t *testing.T) {
	input := "---\n" +
		"name: builder\n" +
		"capability: go-development\n" +
		"harness: claude-code\n" +
		"tools: [read_file]\n" +
		"core: deploy\n" +
		"---\n" +
		"body\n"
	_, err := ParseFaculty([]byte(input))
	if !errors.Is(err, ErrInvalidCore) {
		t.Fatalf("ParseFaculty(core=deploy): got error %v, want it to wrap ErrInvalidCore", err)
	}
}

// TestFacultyRenderOmitsCoreWhenUnset confirms a Faculty created without
// -core (the common case today) round-trips with no "core:" line at all,
// not an empty one — the actual backward-compatibility contract, checked
// against the rendered bytes rather than just the parsed struct.
func TestFacultyRenderOmitsCoreWhenUnset(t *testing.T) {
	f := &Faculty{
		Name:       "builder",
		Capability: "go-development",
		Harness:    "claude-code",
		Tools:      []string{"read_file"},
		Body:       "body",
	}
	if strings.Contains(f.render(), "core:") {
		t.Fatalf("render() with unset Core contains a \"core:\" line:\n%s", f.render())
	}
}

// ── ParseFaculty: malformed input ───────────────────────────────────────

func TestParseFacultyMalformed(t *testing.T) {
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
			_, err := ParseFaculty([]byte(tc.input))
			if err == nil {
				t.Fatalf("ParseFaculty(%q): want error, got nil", tc.name)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseFaculty(%q): got error %v, want it to wrap %v", tc.name, err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("ParseFaculty(%q): error %q does not contain %q", tc.name, err.Error(), tc.wantSub)
			}
		})
	}
}

// ── CLI ──────────────────────────────────────────────────────────────────

func TestFacultyCreateListShow(t *testing.T) {
	dir := t.TempDir()

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file,write_file,exec",
		})
	})
	if code != 0 {
		t.Fatalf("faculty create: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "created") || !strings.Contains(out, "builder.md") {
		t.Fatalf("faculty create: unexpected output:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("faculty list: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "builder") || !strings.Contains(out, "go-development") || !strings.Contains(out, "claude-code") {
		t.Fatalf("faculty list: want builder/go-development/claude-code in output, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyShow([]string{"-faculties-dir=" + dir, "builder"})
	})
	if code != 0 {
		t.Fatalf("faculty show: exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"name:       builder", "capability: go-development", "harness:    claude-code", "read_file, write_file, exec"} {
		if !strings.Contains(out, want) {
			t.Fatalf("faculty show: want %q in output, got:\n%s", want, out)
		}
	}
}

// TestFacultyCreateListShowWithCore mirrors TestFacultyCreateListShow but
// exercises the optional -core flag end to end: created, surfaced in list,
// surfaced in show.
func TestFacultyCreateListShowWithCore(t *testing.T) {
	dir := t.TempDir()

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=leader",
			"-capability=orchestration",
			"-harness=claude-code",
			"-tools=read_file",
			"-core=plan",
		})
	})
	if code != 0 {
		t.Fatalf("faculty create -core=plan: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "core:       plan") {
		t.Fatalf("faculty create -core=plan: want core echoed in output, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("faculty list: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "plan") {
		t.Fatalf("faculty list: want core \"plan\" in output, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyShow([]string{"-faculties-dir=" + dir, "leader"})
	})
	if code != 0 {
		t.Fatalf("faculty show: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "core:       plan") {
		t.Fatalf("faculty show: want core in output, got:\n%s", out)
	}
}

// TestFacultyCreateRejectsInvalidCore confirms the CLI validates -core
// up front rather than writing a file that would fail to parse later.
func TestFacultyCreateRejectsInvalidCore(t *testing.T) {
	dir := t.TempDir()

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=leader",
			"-capability=orchestration",
			"-harness=claude-code",
			"-tools=read_file",
			"-core=deploy",
		})
	})
	if code == 0 {
		t.Fatalf("faculty create -core=deploy: want non-zero exit, output:\n%s", out)
	}
	if !strings.Contains(out, "core value must be one of") {
		t.Fatalf("faculty create -core=deploy: want ErrInvalidCore message, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "leader.md")); !os.IsNotExist(err) {
		t.Fatalf("faculty create -core=deploy: rejected create should not leave a file behind")
	}
}

// TestFacultyCreateListShowWithoutCoreStillWorks confirms omitting -core
// (the pre-existing default path) is unaffected: no core line anywhere,
// list still aligns, show still succeeds. The direct regression check for
// this change's backward-compatibility claim at the CLI layer.
func TestFacultyCreateListShowWithoutCoreStillWorks(t *testing.T) {
	dir := t.TempDir()

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file",
		})
	})
	if code != 0 {
		t.Fatalf("faculty create without -core: exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "core:") {
		t.Fatalf("faculty create without -core: unexpected core line in output:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyShow([]string{"-faculties-dir=" + dir, "builder"})
	})
	if code != 0 {
		t.Fatalf("faculty show: exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "core:") {
		t.Fatalf("faculty show without -core: unexpected core line in output:\n%s", out)
	}
}

func TestFacultyCreateRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	create := func() (string, int) {
		return captureOutput(t, func() int {
			return cmdFacultyCreate([]string{
				"-faculties-dir=" + dir,
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

// TestFacultyListIncludesHandwrittenFile is the automated counterpart of
// this project's own MANUAL_TEST.md pending item: a directory containing
// one Faculty created via the CLI and one authored entirely by hand (the
// real observer.md fixture, not something this test invents) must list
// both correctly — hand-authored Faculties are the expected common case,
// not an edge case.
func TestFacultyListIncludesHandwrittenFile(t *testing.T) {
	dir := t.TempDir()

	_, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file,write_file,exec",
		})
	})
	if code != 0 {
		t.Fatalf("faculty create: want success")
	}

	handwritten, err := os.ReadFile("testdata/observer.md")
	if err != nil {
		t.Fatalf("reading testdata/observer.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observer.md"), handwritten, 0o644); err != nil {
		t.Fatalf("writing hand-authored fixture: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("faculty list: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("faculty list: missing CLI-created builder, got:\n%s", out)
	}
	if !strings.Contains(out, "observer") || !strings.Contains(out, "verification") {
		t.Fatalf("faculty list: missing hand-authored observer, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdFacultyShow([]string{"-faculties-dir=" + dir, "observer"})
	})
	if code != 0 {
		t.Fatalf("faculty show observer: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "query_db") {
		t.Fatalf("faculty show observer: want its real tools in output, got:\n%s", out)
	}
}

// TestFacultyListReportsUnparseableFileWithoutHidingOthers confirms a
// broken hand-edited file is reported clearly by name, and does not take
// down the whole listing or hide a sibling faculty that's fine.
func TestFacultyListReportsUnparseableFileWithoutHidingOthers(t *testing.T) {
	dir := t.TempDir()

	_, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=builder",
			"-capability=go-development",
			"-harness=claude-code",
			"-tools=read_file",
		})
	})
	if code != 0 {
		t.Fatalf("faculty create: want success")
	}

	broken := filepath.Join(dir, "broken.md")
	if err := os.WriteFile(broken, []byte("not a faculty file at all\n"), 0o644); err != nil {
		t.Fatalf("writing broken.md: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code == 0 {
		t.Fatalf("faculty list: want non-zero exit when a file fails to parse")
	}
	if !strings.Contains(out, "broken.md") {
		t.Fatalf("faculty list: want the broken file named in output, got:\n%s", out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("faculty list: a broken file should not hide a valid sibling, got:\n%s", out)
	}
}

func TestFacultyDirResolutionMirrorsDBPath(t *testing.T) {
	t.Setenv("STRATAGEMA_FACULTIES", "")
	if got, want := resolveFacultiesDir(""), filepath.Join(".stratagema", "faculties"); got != want {
		t.Fatalf("resolveFacultiesDir(\"\") with no env = %q, want %q", got, want)
	}

	t.Setenv("STRATAGEMA_FACULTIES", "/tmp/custom-faculties")
	if got, want := resolveFacultiesDir(""), "/tmp/custom-faculties"; got != want {
		t.Fatalf("resolveFacultiesDir(\"\") with env set = %q, want %q", got, want)
	}

	if got, want := resolveFacultiesDir("/explicit/flag/dir"), "/explicit/flag/dir"; got != want {
		t.Fatalf("resolveFacultiesDir(explicit) = %q, want %q (flag should win over env)", got, want)
	}
}
