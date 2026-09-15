package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the permanent regression suite for the push gate itself
// (MANUAL_TEST.md + .githooks/pre-push) — until now that mechanism was
// only verified by hand, ad hoc, against throwaway local bare repos.
// That's exactly the same gap the gate exists to close for the rest of
// the project: a one-off manual check proves a moment in time, not that
// a later edit hasn't broken it. The hook's first version had a real bug
// (its grep matched an illustrative example inside MANUAL_TEST.md's own
// docs, which would have blocked every future push forever) — found by
// running it for real, not by reading it. These tests exercise the
// actual committed `.githooks/pre-push` file byte-for-byte, via real
// `git push` to a real local bare repo, so a regression like that one
// gets caught by `go test` instead of by someone's push failing forever.

// runGit runs a git command in dir, failing the test with full output on
// error — every git invocation here needs to actually succeed for the
// scenario to mean anything.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// newHookRepo sets up a real working-copy git repo with the actual,
// currently-committed .githooks/pre-push installed and wired up via
// core.hooksPath (exactly what GETTING_STARTED.md tells a real user to
// run), a real local bare repo as its "origin," and manualTest as the
// content of MANUAL_TEST.md. Returns the working copy's path.
func newHookRepo(t *testing.T, manualTest string) string {
	t.Helper()

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	hookSrc, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("reading the real .githooks/pre-push: %v", err)
	}

	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	runGit(t, tmp, "init", "-q", "--bare", bare)

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")

	if err := os.MkdirAll(filepath.Join(work, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".githooks", "pre-push"), hookSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "MANUAL_TEST.md"), []byte(manualTest), 0o644); err != nil {
		t.Fatal(err)
	}

	runGit(t, work, "config", "core.hooksPath", ".githooks")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "initial")
	runGit(t, work, "remote", "add", "origin", bare)

	return work
}

// push attempts `git push origin main` (plus any extra args, e.g.
// "--no-verify") from dir and reports the real exit code and combined
// output — never fails the test itself, since a non-zero exit is exactly
// what several of these scenarios expect.
func push(dir string, extraArgs ...string) (code int, output string) {
	args := append([]string{"push"}, extraArgs...)
	args = append(args, "origin", "main")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	switch e := err.(type) {
	case nil:
		return 0, string(out)
	case *exec.ExitError:
		return e.ExitCode(), string(out)
	default:
		return -1, string(out)
	}
}

func TestPrePushHook_CleanPendingSection_Allows(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n(nothing pending right now)\n")
	code, out := push(work)
	if code != 0 {
		t.Fatalf("want push to succeed with a clean Pending section, got exit %d:\n%s", code, out)
	}
}

func TestPrePushHook_MissingFile_Allows(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n(nothing pending right now)\n")
	if err := os.Remove(filepath.Join(work, "MANUAL_TEST.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "remove MANUAL_TEST.md")
	code, out := push(work)
	if code != 0 {
		t.Fatalf("want push to succeed when MANUAL_TEST.md doesn't exist at all, got exit %d:\n%s", code, out)
	}
}

func TestPrePushHook_RealPendingItem_Blocks(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n- [ ] a real item that hasn't been tested yet\n")
	code, out := push(work)
	if code == 0 {
		t.Fatalf("want push blocked with an unresolved pending item, but it succeeded:\n%s", out)
	}
	if !strings.Contains(out, "push blocked") || !strings.Contains(out, "a real item that hasn't been tested yet") {
		t.Fatalf("want the block message to name the actual pending item, got:\n%s", out)
	}
}

func TestPrePushHook_MultiplePendingItems_ReportsAll(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n- [ ] first unresolved item\n- [ ] second unresolved item\n")
	code, out := push(work)
	if code == 0 {
		t.Fatalf("want push blocked with two unresolved items, but it succeeded:\n%s", out)
	}
	if !strings.Contains(out, "first unresolved item") || !strings.Contains(out, "second unresolved item") {
		t.Fatalf("want both pending items named in the output, got:\n%s", out)
	}
}

func TestPrePushHook_CheckedItem_DoesNotBlock(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n- [x] already done and checked off\n")
	code, out := push(work)
	if code != 0 {
		t.Fatalf("want a checked (- [x]) item to not block push, got exit %d:\n%s", code, out)
	}
}

// TestPrePushHook_DocExampleOutsidePending_DoesNotBlock is the direct
// regression test for the hook's own real bug: an illustrative "- [ ]"
// example living in a documentation section (not under "## Pending")
// must never be mistaken for a real pending item. The first version of
// the hook scanned the whole file and failed exactly this case — it
// would have blocked every push, forever, the moment MANUAL_TEST.md's
// own docs showed the checklist syntax by example.
func TestPrePushHook_DocExampleOutsidePending_DoesNotBlock(t *testing.T) {
	content := `# Manual test gate

## Pending

(nothing pending right now)

## How this is meant to work

Add a line like this before you're done:

` + "```" + `
- [ ] example: describe the manual test here
` + "```" + `

Then actually do it, and delete the line.
`
	work := newHookRepo(t, content)
	code, out := push(work)
	if code != 0 {
		t.Fatalf("want push to succeed — the only \"- [ ]\" line is a doc example outside ## Pending, got exit %d:\n%s", code, out)
	}
}

// TestPrePushHook_RealCommittedFile_Allows exercises the actual
// MANUAL_TEST.md as committed in this repo right now, not a
// reconstruction of it — if a future edit to the real file's docs
// accidentally introduces a stray "- [ ]" under the wrong heading, or
// leaves a real pending item unresolved, this catches it directly.
func TestPrePushHook_RealCommittedFile_Allows(t *testing.T) {
	real, err := os.ReadFile("MANUAL_TEST.md")
	if err != nil {
		t.Fatalf("reading the real MANUAL_TEST.md: %v", err)
	}
	work := newHookRepo(t, string(real))
	code, out := push(work)
	if code != 0 {
		t.Fatalf("the real, currently-committed MANUAL_TEST.md should allow a push (no pending items) — if this fails, either resolve a real pending item or check the hook for a regression. Output:\n%s", out)
	}
}

func TestPrePushHook_NoVerify_Bypasses(t *testing.T) {
	work := newHookRepo(t, "# Manual test gate\n\n## Pending\n\n- [ ] deliberately unresolved, to prove --no-verify skips the hook\n")
	code, out := push(work, "--no-verify")
	if code != 0 {
		t.Fatalf("--no-verify should bypass the hook entirely (documented, expected git behavior — see MANUAL_TEST.md), got exit %d:\n%s", code, out)
	}
}

