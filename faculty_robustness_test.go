package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is Faculty's first dedicated adversarial pass — lock.go,
// strategy.go, serve.go, and the runner already had one earlier this
// session; ParseFaculty/ParseFacultyFile/the faculty CLI had not. It
// covers: frontmatter field-type edge cases (every scalar field plus the
// one non-scalar field, tools), delimiter/structure adversarial input,
// body adversarial input, faculty create's file-writing path (including a
// real path-traversal check against an actual temp directory — see
// validFacultyName in faculty.go and TestFacultyCreatePathTraversalNameRejected
// below), and faculty list's directory-scanning robustness.

// ── frontmatter field-type edge cases ───────────────────────────────────

// facultyEdgeCaseValues are reused across name/capability/harness below so
// "what counts as an edge case" is defined once: empty (via the missing
// -field check, covered separately), very long, Unicode, a value
// containing a literal colon (this parser cuts on the *first* colon only,
// so the rest of the line -- including any colons -- must survive
// unmangled), and a value that is itself the delimiter string. None of
// these should error: every field here is a free string with no charset
// restriction by design (see ParseFaculty's own doc comment).
func facultyEdgeCaseValues() map[string]string {
	return map[string]string{
		"long":             strings.Repeat("x", 5000),
		"unicode":          "🎉 emoji, CJK 漢字, RTL مرحبا, combining é",
		"embedded_colon":   "has: a colon: in it",
		"looks_like_delim": "---",
	}
}

func facultyFrontmatterWithField(key, value string) string {
	fields := map[string]string{
		"name":       "builder",
		"capability": "go-development",
		"harness":    "claude-code",
	}
	fields[key] = value
	return "---\n" +
		"name: " + fields["name"] + "\n" +
		"capability: " + fields["capability"] + "\n" +
		"harness: " + fields["harness"] + "\n" +
		"tools: [read_file]\n" +
		"---\n" +
		"body\n"
}

func TestParseFacultyScalarFieldEdgeCases(t *testing.T) {
	for _, field := range []string{"name", "capability", "harness"} {
		for label, value := range facultyEdgeCaseValues() {
			t.Run(field+"/"+label, func(t *testing.T) {
				input := facultyFrontmatterWithField(field, value)
				got, err := ParseFaculty([]byte(input))
				if err != nil {
					t.Fatalf("ParseFaculty with %s=%q: want success, got error: %v", field, value, err)
				}
				var gotVal string
				switch field {
				case "name":
					gotVal = got.Name
				case "capability":
					gotVal = got.Capability
				case "harness":
					gotVal = got.Harness
				}
				if gotVal != value {
					t.Fatalf("ParseFaculty %s = %q, want %q", field, gotVal, value)
				}
			})
		}
	}
}

// TestParseFacultyHarnessTrulyUnvalidated confirms the claim in the task
// brief directly rather than assuming it from reading the code: harness is
// a free string with zero allow-list or format check anywhere in the parse
// path. Nonsense input must parse cleanly.
func TestParseFacultyHarnessTrulyUnvalidated(t *testing.T) {
	nonsense := []string{
		"", // handled by the required-field check below, not here
		"claude-code; rm -rf /",
		"not-a-real-harness-at-all-🤖",
		"http://example.com/harness",
		"0",
		"true",
	}
	for _, h := range nonsense[1:] { // skip "" -- that's the required-field test's job
		t.Run(h, func(t *testing.T) {
			input := facultyFrontmatterWithField("harness", h)
			got, err := ParseFaculty([]byte(input))
			if err != nil {
				t.Fatalf("harness=%q: want no validation error, got: %v", h, err)
			}
			if got.Harness != h {
				t.Fatalf("harness = %q, want %q", got.Harness, h)
			}
		})
	}
}

// TestParseFacultyToolsListEdgeCases exercises the one non-scalar field's
// naive strings.Split(inner, ",") parser directly.
func TestParseFacultyToolsListEdgeCases(t *testing.T) {
	t.Run("hundreds of entries", func(t *testing.T) {
		names := make([]string, 300)
		for i := range names {
			names[i] = "tool_" + strings.Repeat("a", i%7+1)
		}
		toolsLine := "tools: [" + strings.Join(names, ", ") + "]\n"
		input := "---\nname: n\ncapability: c\nharness: h\n" + toolsLine + "---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("300-entry tools list: want success, got: %v", err)
		}
		if len(got.Tools) != 300 {
			t.Fatalf("Tools has %d entries, want 300", len(got.Tools))
		}
	})

	t.Run("duplicate entries are kept, not deduped", func(t *testing.T) {
		input := "---\nname: n\ncapability: c\nharness: h\ntools: [read_file, read_file, read_file]\n---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("duplicate tools: want success, got: %v", err)
		}
		if len(got.Tools) != 3 {
			t.Fatalf("Tools = %v, want 3 entries (no implicit dedup)", got.Tools)
		}
	})

	t.Run("nested brackets: outer layer stripped once, inner literal survives", func(t *testing.T) {
		// Documents actual behavior, verified via a standalone probe before
		// writing this assertion: parseToolsList only ever strips exactly
		// one leading '[' and one trailing ']' -- by design, this format
		// has no nesting (see ParseFaculty's doc comment) -- so "[[a]]"
		// yields one tool literally named "[a]", not a parse error and not
		// two nested tools.
		input := "---\nname: n\ncapability: c\nharness: h\ntools: [[a]]\n---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("tools: [[a]]: want success (no nesting support, but no crash), got: %v", err)
		}
		if len(got.Tools) != 1 || got.Tools[0] != "[a]" {
			t.Fatalf("Tools = %v, want [\"[a]\"]", got.Tools)
		}
	})

	t.Run("entries containing bracket characters ride along as literal text", func(t *testing.T) {
		input := "---\nname: n\ncapability: c\nharness: h\ntools: [a[1], b]end]\n---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("bracket-containing entries: want success, got: %v", err)
		}
		want := []string{"a[1]", "b]end"}
		if len(got.Tools) != len(want) {
			t.Fatalf("Tools = %v, want %v", got.Tools, want)
		}
		for i := range want {
			if got.Tools[i] != want[i] {
				t.Fatalf("Tools[%d] = %q, want %q", i, got.Tools[i], want[i])
			}
		}
	})

	t.Run("a tool name containing a comma cannot be represented -- splits, doesn't error", func(t *testing.T) {
		// Documents an inherent limitation of "no quoting rules beyond
		// what's needed here" (ParseFaculty's own doc comment), not a bug:
		// a comma inside a bracket entry is indistinguishable from a
		// separator, so it silently becomes two entries.
		input := "---\nname: n\ncapability: c\nharness: h\ntools: [a, b, c]\n---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("want success, got: %v", err)
		}
		if len(got.Tools) != 3 {
			t.Fatalf("Tools = %v, want 3 entries", got.Tools)
		}
	})

	t.Run("list that spans what looks like multiple lines is a real parse error, not silently truncated", func(t *testing.T) {
		// Each frontmatter line is processed independently (split on "\n"
		// before any field parsing happens), so a tools value broken across
		// two physical lines never reaches parseToolsList as one value --
		// the first line alone ("[a,") is missing its closing bracket.
		input := "---\nname: n\ncapability: c\nharness: h\ntools: [a,\n  b, c]\n---\nbody\n"
		_, err := ParseFaculty([]byte(input))
		if err == nil {
			t.Fatalf("multi-line-looking tools value: want a parse error, got success")
		}
		// The unclosed first line is itself malformed as a "key: value"
		// line's value (starts with '[' but the isolated line has no ']'),
		// so this surfaces as ErrToolsNotBracketList off that partial line.
		if !strings.Contains(err.Error(), "bracketed list") {
			t.Fatalf("error = %v, want it to mention the bracketed-list requirement", err)
		}
	})
}

// ── frontmatter delimiter/structure adversarial cases ───────────────────

// TestParseFacultyExtraDelimitersAreBodyContent confirms a file with three
// or more "---" lines only ever uses the first two as real delimiters --
// everything after the second is body content, never re-parsed even if it
// looks exactly like another frontmatter block.
func TestParseFacultyExtraDelimitersAreBodyContent(t *testing.T) {
	input := "---\n" +
		"name: real\n" +
		"capability: c\n" +
		"harness: h\n" +
		"tools: [read_file]\n" +
		"---\n" +
		"intro text\n" +
		"---\n" +
		"name: fake\n" +
		"---\n" +
		"more text\n"
	got, err := ParseFaculty([]byte(input))
	if err != nil {
		t.Fatalf("want success, got: %v", err)
	}
	if got.Name != "real" {
		t.Fatalf("Name = %q, want %q (the first block's field, not the embedded fake one)", got.Name, "real")
	}
	wantBody := "intro text\n---\nname: fake\n---\nmore text"
	if got.Body != wantBody {
		t.Fatalf("Body = %q, want %q", got.Body, wantBody)
	}
}

// TestParseFacultyBodyContainingFakeFrontmatterDeep is the same property
// with the embedded look-alike block buried further into a larger body,
// matching the task brief's exact scenario.
func TestParseFacultyBodyContainingFakeFrontmatterDeep(t *testing.T) {
	input := "---\n" +
		"name: real\n" +
		"capability: c\n" +
		"harness: h\n" +
		"tools: [read_file]\n" +
		"---\n" +
		"Paragraph one of real prose.\n\n" +
		"Paragraph two, still prose, several sentences long to look like a\n" +
		"normal body before the embedded block shows up.\n\n" +
		"---\n" +
		"name: fake\n" +
		"capability: fake-cap\n" +
		"harness: fake-harness\n" +
		"tools: [x]\n" +
		"---\n" +
		"Paragraph three, after the fake block.\n"
	got, err := ParseFaculty([]byte(input))
	if err != nil {
		t.Fatalf("want success, got: %v", err)
	}
	if got.Name != "real" || got.Capability != "c" || got.Harness != "h" {
		t.Fatalf("top-level fields were overwritten by the embedded fake block: %+v", got)
	}
	if !strings.Contains(got.Body, "name: fake") {
		t.Fatalf("Body should still literally contain the embedded fake block's text, got: %q", got.Body)
	}
}

// TestParseFacultyDelimiterWhitespaceVariants checks the exact
// strings.TrimSpace(lines[i]) == "---" comparison against the forms a
// real author might type.
func TestParseFacultyDelimiterWhitespaceVariants(t *testing.T) {
	base := "name: n\ncapability: c\nharness: h\ntools: [read_file]\n"

	t.Run("trailing space on delimiter still counts", func(t *testing.T) {
		input := "--- \n" + base + "---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("\"--- \" (trailing space) as opening delimiter: want it accepted (TrimSpace'd), got: %v", err)
		}
		if got.Name != "n" {
			t.Fatalf("Name = %q, want %q", got.Name, "n")
		}
	})

	t.Run("indented delimiter still counts -- intentional leniency, not a bug", func(t *testing.T) {
		// TrimSpace strips leading whitespace too, so an indented "---" is
		// accepted exactly like an unindented one. This is a deliberate
		// side effect of the code's own trimming (read directly, not
		// assumed), documented here rather than treated as a defect: a
		// human hand-editing this file who indents by habit gets a file
		// that still parses, which matches what a reasonable author would
		// expect more than a column-0-only rule would.
		input := "   ---   \n" + base + "---\nbody\n"
		got, err := ParseFaculty([]byte(input))
		if err != nil {
			t.Fatalf("indented \"   ---   \" as opening delimiter: want it accepted, got: %v", err)
		}
		if got.Name != "n" {
			t.Fatalf("Name = %q, want %q", got.Name, "n")
		}
	})

	t.Run("four dashes is not a delimiter", func(t *testing.T) {
		input := "----\n" + base + "---\nbody\n"
		_, err := ParseFaculty([]byte(input))
		if err == nil {
			t.Fatalf("\"----\" as opening line: want ErrMissingOpeningDelimiter, got success")
		}
	})
}

// TestParseFacultyOnlyDelimitersNoFields confirms "---\n---\n" -- delimiters
// present, everything required missing -- fails on the first missing
// required field (name), not some other generic error.
func TestParseFacultyOnlyDelimitersNoFields(t *testing.T) {
	_, err := ParseFaculty([]byte("---\n---\n"))
	if err == nil {
		t.Fatalf("want a missing-field error, got success")
	}
	if !strings.Contains(err.Error(), `missing required frontmatter field "name"`) {
		t.Fatalf("error = %v, want it to name the missing \"name\" field", err)
	}
}

// TestParseFacultyLargeBodyDoesNotChoke reads a several-MB body through the
// real os.ReadFile path (ParseFacultyFile, not just ParseFaculty on an
// in-memory []byte) and confirms it parses correctly and promptly.
// ParseFacultyFile has no size guard at all today (os.ReadFile is
// genuinely unbounded) -- this test documents that it at least works
// correctly for a large well-formed file; whether an unbounded local
// config-file read is itself something to guard is a design question
// reported separately, not something this test asserts on, since faculty
// files are hand-authored local files today with no HTTP entry point
// (unlike serve.go's maxRequestBodyBytes, which bounds actual network
// input).
func TestParseFacultyLargeBodyDoesNotChoke(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.md")

	var b strings.Builder
	b.WriteString("---\nname: huge\ncapability: c\nharness: h\ntools: [read_file]\n---\n")
	line := strings.Repeat("word ", 15) + "\n" // ~75 bytes/line
	for i := 0; i < 60000; i++ {               // ~4.5MB body
		b.WriteString(line)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing large fixture: %v", err)
	}

	got, err := ParseFacultyFile(path)
	if err != nil {
		t.Fatalf("ParseFacultyFile on a ~4.5MB body: want success, got: %v", err)
	}
	if got.Name != "huge" {
		t.Fatalf("Name = %q, want %q", got.Name, "huge")
	}
	if len(got.Body) < 4_000_000 {
		t.Fatalf("Body is only %d bytes, want several MB", len(got.Body))
	}
}

// ── body content adversarial cases ──────────────────────────────────────

func TestParseFacultyEmptyAndWhitespaceOnlyBody(t *testing.T) {
	base := "---\nname: n\ncapability: c\nharness: h\ntools: [read_file]\n---\n"

	t.Run("empty body", func(t *testing.T) {
		got, err := ParseFaculty([]byte(base))
		if err != nil {
			t.Fatalf("want success, got: %v", err)
		}
		if got.Body != "" {
			t.Fatalf("Body = %q, want empty", got.Body)
		}
	})

	t.Run("whitespace-only body", func(t *testing.T) {
		got, err := ParseFaculty([]byte(base + "   \n\t\n   \n"))
		if err != nil {
			t.Fatalf("want success, got: %v", err)
		}
		if got.Body != "" {
			t.Fatalf("Body = %q, want empty (TrimSpace'd)", got.Body)
		}
	})
}

// TestParseFacultyNonUTF8Body confirms invalid UTF-8 byte sequences in the
// body don't panic anything -- ParseFaculty operates on Go strings, which
// can legally hold arbitrary bytes, but a naive rune-aware routine could
// still misbehave on them.
func TestParseFacultyNonUTF8Body(t *testing.T) {
	base := []byte("---\nname: n\ncapability: c\nharness: h\ntools: [read_file]\n---\n")
	invalid := []byte{0xff, 0xfe, 0x00, 0x81, 'o', 'k', 0xc0, 0xc1}
	input := append(append([]byte{}, base...), invalid...)

	got, err := ParseFaculty(input)
	if err != nil {
		t.Fatalf("non-UTF8 body: want success (no panic, no mandatory validity check), got: %v", err)
	}
	if !strings.Contains(got.Body, "ok") {
		t.Fatalf("Body should still contain the valid 'ok' substring around the invalid bytes, got: %q", got.Body)
	}
}

// ── faculty create: file-writing path, path traversal ───────────────────

// TestFacultyCreatePathTraversalNameRejected is the single most important
// test in this file. Before validFacultyName existed, this was run for
// real against an actual temp directory (not asserted from reading the
// code): `faculty create -faculties-dir=$WORK/faculties -name=../evil ...`
// wrote evil.md into $WORK, one directory above $WORK/faculties, silently
// succeeding with exit 0. That is now a rejected, real bug fix -- this
// test locks the fix in and proves the written file genuinely never lands
// outside the faculties directory for a range of traversal shapes.
func TestFacultyCreatePathTraversalNameRejected(t *testing.T) {
	traversalNames := []string{
		"../evil",
		"../../evil",
		"../../../etc/evil",
		"sub/evil",
		"sub/dir/evil",
	}

	for _, name := range traversalNames {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			facultiesDir := filepath.Join(parent, "faculties")
			if err := os.MkdirAll(facultiesDir, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			out, code := captureOutput(t, func() int {
				return cmdFacultyCreate([]string{
					"-faculties-dir=" + facultiesDir,
					"-name=" + name,
					"-capability=x",
					"-harness=y",
					"-tools=a,b",
				})
			})
			if code == 0 {
				t.Fatalf("faculty create -name=%q: want a non-zero exit (rejected), got 0, output:\n%s", name, out)
			}
			if !strings.Contains(out, "path separator") {
				t.Fatalf("faculty create -name=%q: want a clear path-separator error, got:\n%s", name, out)
			}

			// The real assertion: walk the whole parent tree and confirm no
			// file was written anywhere, inside or outside facultiesDir.
			var written []string
			filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					written = append(written, p)
				}
				return nil
			})
			if len(written) != 0 {
				t.Fatalf("faculty create -name=%q: rejected the name but still wrote file(s): %v", name, written)
			}
		})
	}
}

// TestFacultyCreateAbsolutePathNameRejected covers the -name value being an
// absolute path outright, rather than a relative traversal.
func TestFacultyCreateAbsolutePathNameRejected(t *testing.T) {
	parent := t.TempDir()
	facultiesDir := filepath.Join(parent, "faculties")
	if err := os.MkdirAll(facultiesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outsideTarget := filepath.Join(parent, "outside-evil")

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + facultiesDir,
			"-name=" + outsideTarget,
			"-capability=x",
			"-harness=y",
			"-tools=a",
		})
	})
	if code == 0 {
		t.Fatalf("faculty create with an absolute-path -name: want rejection, got exit 0, output:\n%s", out)
	}
	if _, err := os.Stat(outsideTarget + ".md"); err == nil {
		t.Fatalf("faculty create with an absolute-path -name: a file landed at %s.md", outsideTarget)
	}
}

// TestFacultyCreateNullByteNameRejected checks the in-process path only:
// an argv string containing a literal NUL byte cannot actually be passed
// through a real OS process's command line (execve simply cannot represent
// one), so this is exercised by calling cmdFacultyCreate directly with a
// Go string built to contain \x00, not via a subprocess.
func TestFacultyCreateNullByteNameRejected(t *testing.T) {
	dir := t.TempDir()
	name := "evil\x00name"

	out, code := captureOutput(t, func() int {
		return cmdFacultyCreate([]string{
			"-faculties-dir=" + dir,
			"-name=" + name,
			"-capability=x",
			"-harness=y",
			"-tools=a",
		})
	})
	if code == 0 {
		t.Fatalf("faculty create with a NUL byte in -name: want rejection, got exit 0, output:\n%s", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("faculty create with a NUL byte in -name: rejected but still wrote %d file(s)", len(entries))
	}
}

// TestFacultyShowPathTraversalNameRejected is faculty show's read-side
// counterpart: it builds the exact same filepath.Join(dir, name+".md")
// from a user-supplied name, so an unguarded traversal there would let
// `faculty show` read (and print) an arbitrary file elsewhere on disk that
// happens to be frontmatter-shaped. Same validFacultyName guard, same
// property, verified the same way.
func TestFacultyShowPathTraversalNameRejected(t *testing.T) {
	parent := t.TempDir()
	facultiesDir := filepath.Join(parent, "faculties")
	if err := os.MkdirAll(facultiesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A real, valid Faculty file sitting just outside facultiesDir --
	// this is exactly what a successful traversal would have exposed.
	secret := "---\nname: secret\ncapability: c\nharness: h\ntools: [x]\n---\ntop secret body\n"
	if err := os.WriteFile(filepath.Join(parent, "secret.md"), []byte(secret), 0o644); err != nil {
		t.Fatalf("writing secret fixture: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyShow([]string{"-faculties-dir=" + facultiesDir, "../secret"})
	})
	if code == 0 {
		t.Fatalf("faculty show ../secret: want rejection, got exit 0, output:\n%s", out)
	}
	if strings.Contains(out, "top secret body") {
		t.Fatalf("faculty show ../secret: leaked the outside file's contents:\n%s", out)
	}
	if !strings.Contains(out, "path separator") {
		t.Fatalf("faculty show ../secret: want a clear path-separator error, got:\n%s", out)
	}
}

// TestFacultyCreateToolsCLIFlagEdgeCases exercises the CLI-layer tools
// parser (strings.Split(*tools, ",") in cmdFacultyCreate) -- a separate
// code path from parseToolsList's bracketed-list parser above -- through
// the real create->render->reparse round trip.
func TestFacultyCreateToolsCLIFlagEdgeCases(t *testing.T) {
	t.Run("hundreds of entries via -tools", func(t *testing.T) {
		dir := t.TempDir()
		names := make([]string, 400)
		for i := range names {
			names[i] = "t" + strings.Repeat("x", i%5+1)
		}
		toolsFlag := strings.Join(names, ",")

		out, code := captureOutput(t, func() int {
			return cmdFacultyCreate([]string{
				"-faculties-dir=" + dir, "-name=big", "-capability=c", "-harness=h",
				"-tools=" + toolsFlag,
			})
		})
		if code != 0 {
			t.Fatalf("faculty create with 400 tools: want success, got exit %d, output:\n%s", code, out)
		}

		got, err := ParseFacultyFile(filepath.Join(dir, "big.md"))
		if err != nil {
			t.Fatalf("reparsing round trip: %v", err)
		}
		if len(got.Tools) != 400 {
			t.Fatalf("round-tripped Tools has %d entries, want 400", len(got.Tools))
		}
	})

	t.Run("duplicate entries via -tools survive the round trip", func(t *testing.T) {
		dir := t.TempDir()
		out, code := captureOutput(t, func() int {
			return cmdFacultyCreate([]string{
				"-faculties-dir=" + dir, "-name=dup", "-capability=c", "-harness=h",
				"-tools=read_file,read_file,read_file",
			})
		})
		if code != 0 {
			t.Fatalf("want success, output:\n%s", out)
		}
		got, err := ParseFacultyFile(filepath.Join(dir, "dup.md"))
		if err != nil {
			t.Fatalf("reparsing: %v", err)
		}
		if len(got.Tools) != 3 {
			t.Fatalf("Tools = %v, want 3 duplicate entries preserved", got.Tools)
		}
	})

	t.Run("bracket characters inside a CLI tool entry round-trip correctly", func(t *testing.T) {
		// The CLI layer splits only on ',', so an entry may itself contain
		// '[' or ']' -- render() always wraps the whole joined list in
		// exactly one bracket pair and parseToolsList always strips exactly
		// one, so this stays self-consistent as long as no entry contains a
		// literal comma (which the CLI's own comma-split makes impossible
		// to produce in the first place). Verified directly, not assumed.
		dir := t.TempDir()
		out, code := captureOutput(t, func() int {
			return cmdFacultyCreate([]string{
				"-faculties-dir=" + dir, "-name=weird", "-capability=c", "-harness=h",
				"-tools=[a],weird],[nested[x]]",
			})
		})
		if code != 0 {
			t.Fatalf("want success, output:\n%s", out)
		}
		got, err := ParseFacultyFile(filepath.Join(dir, "weird.md"))
		if err != nil {
			t.Fatalf("reparsing bracket-containing tool names: %v", err)
		}
		want := []string{"[a]", "weird]", "[nested[x]]"}
		if len(got.Tools) != len(want) {
			t.Fatalf("Tools = %v, want %v", got.Tools, want)
		}
		for i := range want {
			if got.Tools[i] != want[i] {
				t.Fatalf("Tools[%d] = %q, want %q", i, got.Tools[i], want[i])
			}
		}
	})
}

// ── faculty list: directory-scanning robustness ─────────────────────────

func TestFacultyListManyFiles(t *testing.T) {
	dir := t.TempDir()
	const n = 150
	for i := 0; i < n; i++ {
		content := "---\nname: f" + strconv.Itoa(i) + "\ncapability: c\nharness: h\ntools: [x]\n---\nbody\n"
		path := filepath.Join(dir, "f"+strconv.Itoa(i)+".md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing fixture %d: %v", i, err)
		}
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("faculty list with %d files: want success, got exit %d, output:\n%s", n, code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("faculty list with %d files: got %d output lines, want %d", n, len(lines), n)
	}
}

func TestFacultyListSkipsNonMdFiles(t *testing.T) {
	dir := t.TempDir()
	good := "---\nname: builder\ncapability: c\nharness: h\ntools: [x]\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "builder.md"), []byte(good), 0o644); err != nil {
		t.Fatalf("writing builder.md: %v", err)
	}
	// Non-.md siblings: a README, a dotfile, an extensionless file. None of
	// these should be listed, errored on, or otherwise touched.
	for _, name := range []string{"README.txt", ".DS_Store", "notes", "builder.md.bak"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("irrelevant"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("want success (non-.md siblings should be silently skipped), got exit %d, output:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 listed faculty (non-.md files skipped), got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("want builder.md listed, got:\n%s", out)
	}
}

// TestFacultyListSkipsDirectoryNamedWithMdSuffix confirms e.IsDir() really
// does prevent a subdirectory literally named "something.md" from being
// opened and parsed as a file.
func TestFacultyListSkipsDirectoryNamedWithMdSuffix(t *testing.T) {
	dir := t.TempDir()
	good := "---\nname: builder\ncapability: c\nharness: h\ntools: [x]\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "builder.md"), []byte(good), 0o644); err != nil {
		t.Fatalf("writing builder.md: %v", err)
	}
	trap := filepath.Join(dir, "trap.md")
	if err := os.MkdirAll(trap, 0o755); err != nil {
		t.Fatalf("creating directory named trap.md: %v", err)
	}
	// Put a file inside it so a bug that opens the directory as a file
	// would fail loudly rather than silently reading nothing.
	if err := os.WriteFile(filepath.Join(trap, "inner.md"), []byte("irrelevant"), 0o644); err != nil {
		t.Fatalf("writing inner.md: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	if code != 0 {
		t.Fatalf("want success (directory named *.md should be skipped, not errored on), got exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "trap") {
		t.Fatalf("directory trap.md should not appear in the listing at all, got:\n%s", out)
	}
	if !strings.Contains(out, "builder") {
		t.Fatalf("want builder still listed, got:\n%s", out)
	}
}

// TestFacultyListHandlesSymlinks covers a symlink to a valid Faculty file
// (should list normally, under the symlink's own name) and a broken
// symlink (should be reported as a parse failure like any other unreadable
// file, not crash the whole listing).
func TestFacultyListHandlesSymlinks(t *testing.T) {
	dir := t.TempDir()
	realTarget := filepath.Join(t.TempDir(), "real.md")
	good := "---\nname: linked\ncapability: c\nharness: h\ntools: [x]\n---\nbody\n"
	if err := os.WriteFile(realTarget, []byte(good), 0o644); err != nil {
		t.Fatalf("writing real target: %v", err)
	}

	if err := os.Symlink(realTarget, filepath.Join(dir, "link.md")); err != nil {
		t.Skipf("symlink creation not available in this environment: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "broken.md")); err != nil {
		t.Fatalf("creating broken symlink: %v", err)
	}

	out, code := captureOutput(t, func() int {
		return cmdFacultyList([]string{"-faculties-dir=" + dir})
	})
	// Non-zero is expected here (broken.md fails to parse), but it must not
	// crash and must not hide the valid symlinked entry.
	if !strings.Contains(out, "linked") {
		t.Fatalf("valid symlink to a real Faculty file: want it listed, got exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "broken.md") {
		t.Fatalf("broken symlink: want it reported by name as a skip, got:\n%s", out)
	}
}
