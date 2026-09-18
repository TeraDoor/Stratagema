package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildRootBinary compiles the real `stratagema` CLI from the repo root
// (".." relative to this package) once per test run -- the same pattern
// ../e2e_test.go's buildBinary already uses for its own real-process
// tests, mirrored here rather than imported (see main.go's package doc
// comment: the root package is `package main` and cannot be imported).
// This is what stands in for "a real local test store": a real compiled
// binary, a real SQLite file, a real `stratagema serve` HTTP server --
// not a mock of any of it.
func buildRootBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stratagema-root")
	cmd := exec.Command("go", "build", "-o", bin, "..")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building root stratagema binary: %v\n%s", err, out)
	}
	return bin
}

// pickFreePort asks the OS for a free TCP port and releases it
// immediately -- the standard, small-race-accepted way to pre-choose a
// port for a subprocess server to bind to when the server itself (serve.go)
// takes a fixed -port flag rather than supporting OS-assigned :0 with a
// way to read back what it actually bound.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runCLI runs bin with args and returns trimmed stdout, failing the test
// on a non-zero exit or spawn error -- used only for the setup steps
// (identity create, strategy create), not for anything under test.
func runCLI(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// firstIDField parses the "created <id>\n  ..." shape every `... create`
// command in this project prints (identity.go's cmdIdentityCreate,
// strategy.go's cmdStrategyCreate) and returns just the id.
func firstIDField(cliOutput string) string {
	line := strings.SplitN(cliOutput, "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// testCoordinator is a real `stratagema serve` subprocess, backed by a
// real local SQLite file, with one real identity and one real strategy
// already created against it via the real CLI -- everything a test needs
// to exercise the runner's actual logic (run(), in this same package)
// against a real Coordinator API, and to independently verify afterward
// what actually landed, without trusting the runner's own stdout.
type testCoordinator struct {
	baseURL    string
	identityID string
	strategyID string
	rootBin    string
}

func startTestCoordinator(t *testing.T) *testCoordinator {
	t.Helper()
	rootBin := buildRootBinary(t)
	dbFile := filepath.Join(t.TempDir(), "runner-test.db")

	identOut := runCLI(t, rootBin, "identity", "create", "-db="+dbFile, "-label=runner-test-identity")
	identityID := firstIDField(identOut)
	if identityID == "" {
		t.Fatalf("could not parse identity id from: %q", identOut)
	}

	stratOut := runCLI(t, rootBin, "strategy", "create", "-db="+dbFile, "-name=runner-test", "-thesis=prove the runner's exit-code-to-event-kind logic against a real coordinator")
	strategyID := firstIDField(stratOut)
	if strategyID == "" {
		t.Fatalf("could not parse strategy id from: %q", stratOut)
	}

	port := pickFreePort(t)
	cmd := exec.Command(rootBin, "serve", "-db="+dbFile, "-port="+strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting stratagema serve: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, baseURL)

	return &testCoordinator{baseURL: baseURL, identityID: identityID, strategyID: strategyID, rootBin: rootBin}
}

func waitForHealth(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("stratagema serve at %s never became healthy", baseURL)
}

// events reads the real event log back from the real running coordinator,
// independent of anything the runner itself claimed on stdout -- this is
// the actual proof this task requires.
func (tc *testCoordinator) events(t *testing.T) []strategyEvent {
	t.Helper()
	c := &coordinatorClient{baseURL: tc.baseURL, http: http.DefaultClient}
	var out []strategyEvent
	if err := c.do(http.MethodGet, "/strategies/"+tc.strategyID+"/events", nil, &out); err != nil {
		t.Fatalf("reading back events: %v", err)
	}
	return out
}

// writeScript writes an executable shell script to dir and returns its
// path -- used to give a failing "harness" real, controlled stderr and a
// real, controlled non-zero exit code, the way the task's manual test
// plan also uses (an echo/false-based command is a legitimate stand-in
// for a real harness here: the property under test is the runner's
// exit-code handling, not any real AI harness's output quality).
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeScript: %v", err)
	}
	return path
}

const testFacultyBody = `---
name: test-faculty
capability: testing
harness: test-harness
tools: [read_file]
---

Prose body used as the prompt base in runner tests.
`

func writeFaculty(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-faculty.md")
	if err := os.WriteFile(path, []byte(testFacultyBody), 0o644); err != nil {
		t.Fatalf("writeFaculty: %v", err)
	}
	return path
}

// ── the actual tests ────────────────────────────────────────────────────

func TestRun_MissingRequiredFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-faculty=x"}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed)", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "missing required flag") {
		t.Fatalf("stderr = %q, want it to mention missing required flags", stderr.String())
	}
}

func TestRun_MissingFacultyFileFailsCleanly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=/nonexistent/path/does-not-exist.md",
		"-harness-cmd=echo",
		"-db=http://127.0.0.1:1", // never reached -- faculty parse must fail first
		"-identity=whoever",
		"-strategy=whichever",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed)", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "faculty:") {
		t.Fatalf("stderr = %q, want a clean faculty error, not a panic", stderr.String())
	}
}

func TestRun_MalformedFacultyFileFailsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed.md")
	if err := os.WriteFile(path, []byte("no frontmatter here at all"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(stderr.String(), "opening ---") {
		t.Fatalf("stderr = %q, want the real missing-delimiter error", stderr.String())
	}
}

func TestRun_RejectsLocalDBPath(t *testing.T) {
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=/some/local/file.db",
		"-identity=whoever",
		"-strategy=whichever",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed)", code, exitRunnerFailed)
	}
	if !strings.Contains(stderr.String(), "http://") {
		t.Fatalf("stderr = %q, want it to explain -db must be a coordinator URL", stderr.String())
	}
}

// TestRun_HarnessSuccessLogsStepCompleted is this task's central claim,
// proven for real: a real `stratagema serve` (real SQLite file behind it),
// a real strategy, a real harness process (echo) that really exits 0, run
// through the runner's actual run() -- then verified by reading the
// events back from the coordinator directly, not by trusting run()'s own
// stdout.
func TestRun_HarnessSuccessLogsStepCompleted(t *testing.T) {
	tc := startTestCoordinator(t)
	facultyPath := writeFaculty(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=" + tc.baseURL,
		"-identity=" + tc.identityID,
		"-strategy=" + tc.strategyID,
		"-task=prove the success path",
	}, &stdout, &stderr)

	if code != exitOK {
		t.Fatalf("run() code = %d, want %d (exitOK); stderr=%s", code, exitOK, stderr.String())
	}

	events := tc.events(t)
	if len(events) < 2 {
		t.Fatalf("want at least 2 events (step_started, step_completed), got %d: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if last.Kind != "step_completed" {
		t.Fatalf("last event kind = %q, want step_completed (full log: %+v)", last.Kind, events)
	}
	if !strings.Contains(last.Note, "harness exited 0") {
		t.Fatalf("step_completed note = %q, want it to say the harness exited 0", last.Note)
	}
	if !strings.Contains(last.Note, "prove the success path") {
		t.Fatalf("step_completed note = %q, want it to contain the echoed prompt (including -task)", last.Note)
	}
	if events[0].Kind != "step_started" {
		t.Fatalf("first event kind = %q, want step_started", events[0].Kind)
	}
}

// TestRun_HarnessFailureLogsFinding is the other half of the same claim:
// a real non-zero exit becomes a finding, never a step_completed, and the
// runner's own exit code (1) is distinct from a runner-level failure (2).
func TestRun_HarnessFailureLogsFinding(t *testing.T) {
	tc := startTestCoordinator(t)
	facultyPath := writeFaculty(t)
	failScript := writeScript(t, "echo synthetic-stderr-output 1>&2\nexit 7")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=" + failScript,
		"-db=" + tc.baseURL,
		"-identity=" + tc.identityID,
		"-strategy=" + tc.strategyID,
		"-task=prove the failure path",
	}, &stdout, &stderr)

	if code != exitHarnessFailed {
		t.Fatalf("run() code = %d, want %d (exitHarnessFailed); stdout=%s stderr=%s", code, exitHarnessFailed, stdout.String(), stderr.String())
	}

	events := tc.events(t)
	if len(events) < 2 {
		t.Fatalf("want at least 2 events (step_started, finding), got %d: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if last.Kind != "finding" {
		t.Fatalf("last event kind = %q, want finding, never step_completed, on a non-zero exit (full log: %+v)", last.Kind, events)
	}
	if !strings.Contains(last.Note, "exit code 7") {
		t.Fatalf("finding note = %q, want it to state the real exit code (7)", last.Note)
	}
	if !strings.Contains(last.Note, "synthetic-stderr-output") {
		t.Fatalf("finding note = %q, want it to include the captured stderr", last.Note)
	}
}

// TestRun_UnreachableCoordinatorIsARunnerFailure proves the exit-code
// distinction this task requires: the coordinator being unreachable is a
// different failure from the harness itself failing, and must never be
// reported as either exitOK or exitHarnessFailed.
func TestRun_UnreachableCoordinatorIsARunnerFailure(t *testing.T) {
	facultyPath := writeFaculty(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-faculty=" + facultyPath,
		"-harness-cmd=echo",
		"-db=http://127.0.0.1:1", // nothing listens on port 1
		"-identity=whoever",
		"-strategy=whichever",
		"-task=irrelevant",
	}, &stdout, &stderr)
	if code != exitRunnerFailed {
		t.Fatalf("code = %d, want %d (exitRunnerFailed) when the coordinator can't be reached", code, exitRunnerFailed)
	}
}

// ── small unit tests for the note-building/truncation logic ─────────────

func TestBuildFailureNote(t *testing.T) {
	note := buildFailureNote(3, []byte("boom"))
	if !strings.Contains(note, "exit code 3") || !strings.Contains(note, "boom") {
		t.Fatalf("buildFailureNote = %q", note)
	}
}

func TestTruncateForNoteCapsLength(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxEventNoteBytes+500)
	text, truncated := truncateForNote(big)
	if !truncated {
		t.Fatalf("want truncated=true for input over the cap")
	}
	if len(text) != maxEventNoteBytes {
		t.Fatalf("len(text) = %d, want %d", len(text), maxEventNoteBytes)
	}
	note := buildCompletedNote(big)
	if !strings.Contains(note, "truncated") {
		t.Fatalf("buildCompletedNote note doesn't mention truncation: %q", note[:200])
	}
}

func TestParseFacultyRoundTrip(t *testing.T) {
	f, err := parseFaculty([]byte(testFacultyBody))
	if err != nil {
		t.Fatalf("parseFaculty: %v", err)
	}
	if f.Name != "test-faculty" || f.Harness != "test-harness" {
		t.Fatalf("f = %+v", f)
	}
	if !strings.Contains(f.Body, "Prose body used as the prompt base") {
		t.Fatalf("f.Body = %q", f.Body)
	}
}

func TestRunHarnessCapturesRealExitCodeAndOutput(t *testing.T) {
	res := runHarness("echo", "hello world")
	if res.exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", res.exitCode)
	}
	if !strings.Contains(string(res.stdout), "hello world") {
		t.Fatalf("stdout = %q, want it to contain the prompt", string(res.stdout))
	}

	failScript := writeScript(t, "exit 42")
	res = runHarness(failScript, "unused")
	if res.exitCode != 42 {
		t.Fatalf("exitCode = %d, want 42", res.exitCode)
	}
	if res.startErr != nil {
		t.Fatalf("startErr = %v, want nil (the process started and ran; it just exited non-zero)", res.startErr)
	}
}
