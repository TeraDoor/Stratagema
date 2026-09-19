package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file is the broader robustness pass: Faculty/-task field-type edge
// cases (including a real attempt to break the sh -c 'cmd "$@"' sh <prompt>
// injection-prevention idiom in harness.go, not just trusting its doc
// comment), -harness-cmd shapes, coordinator-communication edge cases, and
// an exhaustive exit-code matrix. Same no-mocks pattern as main_test.go:
// real subprocesses, a real compiled stratagema binary, a real `serve`.

// ── helpers additional to main_test.go's ────────────────────────────────

// writeFacultyCustom is writeFaculty's parametrized twin, for tests that
// need control over name/harness/body (long, unicode, empty, adversarial).
func writeFacultyCustom(t *testing.T, name, harness, body string) string {
	t.Helper()
	content := fmt.Sprintf("---\nname: %s\nharness: %s\n---\n\n%s\n", name, harness, body)
	path := filepath.Join(t.TempDir(), "custom-faculty.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFacultyCustom: %v", err)
	}
	return path
}

// ── 1. shell-injection resistance -- the single most safety-relevant
// property in this tool. Directly exercises runHarness (the actual idiom),
// not a simulation of it. ────────────────────────────────────────────────

func TestRunHarness_ShellInjectionResistance(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned-marker")
	payloads := []string{
		"backtick`touch " + marker + "`end",
		"cmdsub$(touch " + marker + ")end",
		"semicolon; touch " + marker,
		"and&&touch " + marker,
		"or||touch " + marker,
		"pipe|touch " + marker,
		`unmatched "double quote`,
		`unmatched 'single quote`,
		"literal-dollar-at $@ end",
		"dollar-star $* end",
		"newline\nembedded",
		"tab\tembedded",
		`$(rm -rf /tmp/should-never-run-` + marker + `)`,
		"héllo wörld 你好 🎉 unicode",
		strings.Repeat("A", 8000), // several KB, well past maxEventNoteBytes
	}

	for _, payload := range payloads {
		t.Run(shortName(payload), func(t *testing.T) {
			res := runHarness("printf '%s'", payload)
			if res.startErr != nil {
				t.Fatalf("startErr = %v, want nil", res.startErr)
			}
			if res.exitCode != 0 {
				t.Fatalf("exitCode = %d, want 0; stderr=%q", res.exitCode, res.stderr)
			}
			if string(res.stdout) != payload {
				t.Fatalf("stdout = %q, want exactly the payload %q (injection-prevention idiom did not hold byte-for-byte)", res.stdout, payload)
			}
		})
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("marker file %s exists -- shell injection actually executed a side-effecting command from inside the prompt", marker)
	}
}

func shortName(s string) string {
	if len(s) > 24 {
		s = s[:24] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '_'
		}
		return r
	}, s)
}

// TestRunHarness_NULByteInPromptFailsCleanly: a NUL byte can legitimately
// appear in a Faculty file's raw bytes (os.ReadFile preserves it), and Go
// strings permit it, but no POSIX argv element ever can (execve requires
// NUL-terminated C strings). Confirms this is reported as an honest
// startErr (runner-level failure), never a panic or a silently-truncated
// prompt.
func TestRunHarness_NULByteInPromptFailsCleanly(t *testing.T) {
	res := runHarness("echo", "hello\x00world")
	if res.startErr == nil {
		t.Fatalf("want a non-nil startErr for a NUL byte in the prompt argv, got exitCode=%d stdout=%q", res.exitCode, res.stdout)
	}
}

// ── 2. Faculty file field-type edge cases ───────────────────────────────

func TestFaculty_LongBodyPassesThroughIntact(t *testing.T) {
	body := strings.Repeat("Lorem ipsum dolor sit amet. ", 300) // several KB
	facultyPath := writeFacultyCustom(t, "long-body-faculty", "test-harness", body)

	fac, err := parseFacultyFile(facultyPath)
	if err != nil {
		t.Fatalf("parseFacultyFile: %v", err)
	}
	if !strings.Contains(fac.Body, "Lorem ipsum") {
		t.Fatalf("parsed body lost content, len=%d", len(fac.Body))
	}

	prompt := buildPrompt(fac, "task")
	res := runHarness("printf '%s'", prompt)
	if res.startErr != nil || res.exitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(string(res.stdout), fac.Body) {
		t.Fatalf("the long body did not survive the full round trip through argv; got %d bytes out, wanted body of %d bytes included", len(res.stdout), len(fac.Body))
	}
}

func TestFaculty_UnicodeBody(t *testing.T) {
	body := "日本語のテスト héllo wörld Ñoño Москва 🎉🚀 emoji test"
	facultyPath := writeFacultyCustom(t, "unicode-faculty", "test-harness", body)
	fac, err := parseFacultyFile(facultyPath)
	if err != nil {
		t.Fatalf("parseFacultyFile: %v", err)
	}
	if fac.Body != body {
		t.Fatalf("Body = %q, want %q", fac.Body, body)
	}
	res := runHarness("printf '%s'", buildPrompt(fac, "task"))
	if res.exitCode != 0 || !strings.Contains(string(res.stdout), body) {
		t.Fatalf("unicode body did not survive: exitCode=%d stdout=%q", res.exitCode, res.stdout)
	}
}

func TestFaculty_EmptyBodyParsesCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-body.md")
	content := "---\nname: empty-body\nharness: test-harness\n---\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fac, err := parseFacultyFile(path)
	if err != nil {
		t.Fatalf("parseFacultyFile: %v, want a clean parse of a syntactically valid, empty-body faculty", err)
	}
	if fac.Body != "" {
		t.Fatalf("Body = %q, want empty", fac.Body)
	}
	// buildPrompt must not panic or produce something degenerate on an
	// empty body.
	prompt := buildPrompt(fac, "some task")
	if !strings.Contains(prompt, "# Task") || !strings.Contains(prompt, "some task") {
		t.Fatalf("prompt = %q, malformed for empty-body faculty", prompt)
	}
}

func TestFaculty_UnreadableFilePermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced against root")
	}
	path := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(path, []byte(testFacultyBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	_, err := parseFacultyFile(path)
	if err == nil {
		t.Fatal("want a permission error, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want it to mention permission denied", err)
	}

	// And through run(), it must be a clean exitRunnerFailed, not a panic.
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + path,
		"-harness-cmd=echo",
		"-db=http://127.0.0.1:1",
		"-identity=whoever",
		"-strategy=whichever",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed)", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr = %q, want it to surface the permission error", stderr.String())
	}
}

// ── 3. -task field-type edge cases ──────────────────────────────────────

func TestRun_EmptyTaskIsTreatedAsMissingFlag(t *testing.T) {
	// main.go's missing-flag check treats "" as absent for every required
	// flag including -task -- confirms an empty -task can never silently
	// reach buildPrompt as a blank task section.
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=http://127.0.0.1:1",
		"-identity=whoever",
		"-strategy=whichever",
		"-task=",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed)", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "-task") {
		t.Fatalf("stderr = %q, want it to name -task as missing", stderr.String())
	}
}

func TestBuildPrompt_TaskAdversarialAndUnicodeSurviveIntact(t *testing.T) {
	fac := &Faculty{Name: "f", Harness: "h", Body: "body"}
	task := "task with `backtick` $(cmdsub) ; semi && and || or | pipe $@ 'quote\" 日本語 🎉 " + strings.Repeat("x", 5000)
	prompt := buildPrompt(fac, task)
	res := runHarness("printf '%s'", prompt)
	if res.exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if string(res.stdout) != prompt {
		t.Fatalf("adversarial -task content did not survive the harness round trip intact")
	}
}

// ── 4. -harness-cmd edge cases ──────────────────────────────────────────

func TestRunHarness_NonexistentCommandShapes(t *testing.T) {
	cases := []struct {
		name       string
		harnessCmd string
	}{
		{"bare nonexistent", "this-command-truly-does-not-exist-anywhere"},
		{"nonexistent with flags", "this-command-truly-does-not-exist-anywhere --flag value"},
		{"nonexistent absolute path", "/no/such/path/at/all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runHarness(tc.harnessCmd, "prompt")
			if res.startErr != nil {
				t.Fatalf("startErr = %v, want nil -- sh itself starts fine, it just can't find %q (should surface as exit 127, not a start failure)", res.startErr, tc.harnessCmd)
			}
			if res.exitCode != 127 {
				t.Fatalf("exitCode = %d, want 127 (sh: command not found)", res.exitCode)
			}
		})
	}
}

func TestRunHarness_NestedShellQuotingInHarnessCmd(t *testing.T) {
	// harness-cmd carrying its own quoting/subshell -- confirms the outer
	// sh -c 'cmd "$@"' sh <prompt> idiom composes correctly with an
	// operator command that is itself a full shell invocation.
	res := runHarness(`sh -c "exit 3"`, "unused prompt")
	if res.startErr != nil {
		t.Fatalf("startErr = %v", res.startErr)
	}
	if res.exitCode != 3 {
		t.Fatalf("exitCode = %d, want 3", res.exitCode)
	}
}

func TestRunHarness_HarnessCmdWithOwnEmbeddedArgs(t *testing.T) {
	res := runHarness("echo -n", "sentinel-value")
	if res.startErr != nil || res.exitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(string(res.stdout), "sentinel-value") {
		t.Fatalf("stdout = %q, want it to contain the prompt", res.stdout)
	}
}

// TestRunHarness_StdinReadDoesNotHang: a harness that reads all of stdin
// (cat) must not hang the runner. runHarness never sets cmd.Stdin, and
// os/exec's own documented behavior for a nil Stdin is to connect the
// child to the null device -- so a stdin-reading harness sees immediate
// EOF, not a live pipe with nothing arriving. Confirmed here for real
// (with a hard test-level deadline, not a trust of the docs) rather than
// assumed: this is NOT a hang risk, and adding a speculative timeout for
// it would be an unrequested feature for a gap that doesn't exist.
func TestRunHarness_StdinReadDoesNotHang(t *testing.T) {
	script := writeScript(t, "cat >/dev/null")
	done := make(chan harnessResult, 1)
	go func() { done <- runHarness(script, "prompt") }()
	select {
	case res := <-done:
		if res.startErr != nil {
			t.Fatalf("startErr = %v", res.startErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHarness hung for 5s on a harness reading stdin -- nil Stdin is not being connected to the null device as expected")
	}
}

// TestRunHarness_LargeOutputIsBoundedInMemory: a harness producing multiple
// MB of stdout must not be captured unbounded before truncateForNote ever
// runs. This asserts on harnessResult.stdout's own length, independent of
// the note-building layer, so it actually proves the capture itself is
// bounded rather than merely the logged note.
func TestRunHarness_LargeOutputIsBoundedInMemory(t *testing.T) {
	const hugeMB = 8
	script := writeScript(t, fmt.Sprintf(`yes A | head -c %d`, hugeMB*1024*1024))
	res := runHarness(script, "prompt")
	if res.startErr != nil || res.exitCode != 0 {
		t.Fatalf("res.startErr=%v exitCode=%d stderr=%q", res.startErr, res.exitCode, res.stderr)
	}
	if len(res.stdout) >= hugeMB*1024*1024 {
		t.Fatalf("len(res.stdout) = %d, want it capped well below the %d MB the harness actually wrote -- stdout capture is unbounded", len(res.stdout), hugeMB)
	}
	// The note built from it must still be correctly capped too (this part
	// already worked before any fix).
	note := buildCompletedNote(res.stdout)
	if !strings.Contains(note, "truncated") {
		t.Fatalf("note doesn't mention truncation for %d bytes of captured output", len(res.stdout))
	}
}

func TestBoundedWriter_CapsAndReportsFullWrite(t *testing.T) {
	w := &boundedWriter{limit: 10}
	n, err := w.Write([]byte("0123456789EXTRA"))
	if err != nil {
		t.Fatalf("err = %v, want nil (a full capture buffer must never look like a write error to the caller)", err)
	}
	if n != len("0123456789EXTRA") {
		t.Fatalf("n = %d, want %d (must report the full write consumed even though excess bytes were dropped)", n, len("0123456789EXTRA"))
	}
	if w.buf.String() != "0123456789" {
		t.Fatalf("buf = %q, want exactly the first 10 bytes", w.buf.String())
	}
	// A second write past the cap must be a pure no-op, not a panic or a
	// negative-length slice.
	n2, err2 := w.Write([]byte("more"))
	if err2 != nil || n2 != 4 {
		t.Fatalf("n2=%d err2=%v, want n2=4 err2=nil", n2, err2)
	}
	if w.buf.Len() != 10 {
		t.Fatalf("buf.Len() = %d, want still 10", w.buf.Len())
	}
}

// ── 5. Coordinator-communication edge cases ─────────────────────────────

func TestRun_UnreachableCoordinatorErrorIsClear(t *testing.T) {
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=http://127.0.0.1:1",
		"-identity=whoever",
		"-strategy=whichever",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "identity verification failed") {
		t.Fatalf("stderr = %q, want a clear identity-verification-stage error naming the failure, not a bare/confusing message", stderr.String())
	}
}

func TestRun_NonexistentStrategyFailsCleanly(t *testing.T) {
	tc := startTestCoordinator(t)
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=" + tc.baseURL,
		"-identity=" + tc.identityID,
		"-strategy=strategy-that-does-not-exist",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed) for a nonexistent strategy", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q, want a clean real 'not found' error, not a confusing one", stderr.String())
	}
}

// TestRun_NonexistentIdentitySucceeds documents real, deliberate behavior
// (identity.go's VerifyIdentityToken doc comment): an identity ID with no
// row at all is "unprotected, same as always" -- Identity is deliberately
// just a label, and no call site has ever required `identity create` to
// run first. A garbage -identity is therefore NOT an error case in this
// system; confirming it here (not just trusting the comment) so this
// runner's own edge-case coverage doesn't quietly assume otherwise.
func TestRun_NonexistentIdentitySucceeds(t *testing.T) {
	tc := startTestCoordinator(t)
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=" + tc.baseURL,
		"-identity=identity-that-was-never-created",
		"-strategy=" + tc.strategyID,
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d, want %d (exitOK); stderr=%s", code, exitOK, stderr.String())
	}
}

// TestRun_CoordinatorDiesMidRun is the mid-run-unreachable case: the
// coordinator accepts step_started, then the harness process itself kills
// the coordinator before the runner can log the completion event. Must be
// reported as exitRunnerFailed (2) -- a coordinator problem -- never
// exitOK (the harness genuinely did finish) or exitHarnessFailed (the
// harness is not at fault).
func TestRun_CoordinatorDiesMidRun(t *testing.T) {
	rootBin := buildRootBinary(t)
	dbFile := filepath.Join(t.TempDir(), "midrun.db")
	identOut := runCLI(t, rootBin, "identity", "create", "-db="+dbFile, "-label=midrun-identity")
	identityID := firstIDField(identOut)
	stratOut := runCLI(t, rootBin, "strategy", "create", "-db="+dbFile, "-name=midrun", "-thesis=mid-run coordinator death")
	strategyID := firstIDField(stratOut)

	port := pickFreePort(t)
	serveCmd := exec.Command(rootBin, "serve", "-db="+dbFile, "-port="+strconv.Itoa(port))
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("starting stratagema serve: %v", err)
	}
	t.Cleanup(func() { serveCmd.Process.Kill(); serveCmd.Wait() })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, baseURL)

	killerScript := writeScript(t, fmt.Sprintf("kill -9 %d 2>/dev/null; sleep 0.3; exit 0", serveCmd.Process.Pid))
	facultyPath := writeFaculty(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=" + killerScript,
		"-db=" + baseURL,
		"-identity=" + identityID,
		"-strategy=" + strategyID,
		"-task=kill the coordinator mid-run",
	}, &stdout, &stderr)

	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed) when the coordinator dies between step_started and step_completed; stdout=%s stderr=%s", code, exitRunnerFailed, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "step_started logged") {
		t.Fatalf("stdout = %q, want step_started to have actually succeeded before the coordinator died", stdout.String())
	}
	if !strings.Contains(stderr.String(), "step_completed") {
		t.Fatalf("stderr = %q, want it to specifically name step_completed as what failed to log", stderr.String())
	}
}

// ── 6. Exit code matrix, exhaustive ─────────────────────────────────────

func TestRun_ExitCodeMatrix(t *testing.T) {
	tc := startTestCoordinator(t)
	facultyPath := writeFaculty(t)

	base := func(overrides map[string]string) []string {
		args := map[string]string{
			"-faculty":     facultyPath,
			"-harness-cmd": "echo",
			"-db":          tc.baseURL,
			"-identity":    tc.identityID,
			"-strategy":    tc.strategyID,
			"-task":        "exit code matrix",
		}
		for k, v := range overrides {
			args[k] = v
		}
		var out []string
		for k, v := range args {
			out = append(out, k+"="+v)
		}
		return out
	}

	missingPath := filepath.Join(t.TempDir(), "does-not-exist.md")
	malformedPath := filepath.Join(t.TempDir(), "malformed.md")
	os.WriteFile(malformedPath, []byte("not a faculty file"), 0o644)
	failScript := writeScript(t, "exit 5")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"harness succeeds", base(nil), exitOK},
		{"harness fails non-zero", base(map[string]string{"-harness-cmd": failScript}), exitHarnessFailed},
		{"harness command not found (sh exit 127)", base(map[string]string{"-harness-cmd": "no-such-binary-xyz"}), exitHarnessFailed},
		{"missing required flags", []string{"-faculty=" + facultyPath}, exitRunnerFailed},
		{"missing faculty file", base(map[string]string{"-faculty": missingPath}), exitRunnerFailed},
		{"malformed faculty file", base(map[string]string{"-faculty": malformedPath}), exitRunnerFailed},
		{"bad -db (not a URL)", base(map[string]string{"-db": "/local/file.db"}), exitRunnerFailed},
		{"unreachable coordinator", base(map[string]string{"-db": "http://127.0.0.1:1"}), exitRunnerFailed},
		{"nonexistent strategy", base(map[string]string{"-strategy": "no-such-strategy"}), exitRunnerFailed},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(c.args, &stdout, &stderr)
			if code != c.want {
				t.Fatalf("code = %d, want %d; stdout=%s stderr=%s", code, c.want, stdout.String(), stderr.String())
			}
		})
	}

	// NUL-byte-in-prompt-causes-a-genuine-process-start-failure, exercised
	// via a Faculty body (the one real way a NUL byte reaches the prompt)
	// rather than a flag string.
	t.Run("harness process cannot even start (NUL byte in prompt)", func(t *testing.T) {
		nulFacultyPath := filepath.Join(t.TempDir(), "nul-faculty.md")
		content := "---\nname: nul-faculty\nharness: test-harness\n---\n\nbody with a NUL \x00 byte\n"
		if err := os.WriteFile(nulFacultyPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := run(base(map[string]string{"-faculty": nulFacultyPath}), &stdout, &stderr)
		if code != exitRunnerFailed {
			t.Fatalf("code = %d, want %d (exitRunnerFailed) -- the harness process never starts at all", code, exitRunnerFailed)
		}
		if !strings.Contains(stderr.String(), "failed to invoke harness command") {
			t.Fatalf("stderr = %q, want it to say the harness command could not be invoked", stderr.String())
		}
	})

	// harness succeeds but the coordinator is gone by the time the
	// completion event is logged -- covered in full by
	// TestRun_CoordinatorDiesMidRun above; included here in spirit via
	// that separate, more heavily-instrumented test rather than
	// duplicated inline.
}
