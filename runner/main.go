// Command stratagema-runner is a genuinely separate binary from
// `stratagema` itself, built and run independently (`go build -o
// bin/stratagema-runner ./runner`, then invoked on its own -- it is never
// wired into stratagema's own main.go command switch). Stratagema
// coordinates agents; its own README states plainly that it does not run
// them ("No forever-running orchestrator... does not run your agents for
// you"). This tool is the first real, minimal step past that boundary:
// it actually launches one agent harness (Claude Code, Codex, ...) for
// one Faculty against one strategy, and reports back -- but it does so as
// an ordinary client of Stratagema's existing public Coordinator API (the
// same HTTP surface `remote.go`'s RemoteStore and `serve.go`'s routes
// expose to any other remote client), never by importing Stratagema's
// Store/Coordinator types and acting as if it were part of the core.
//
// That boundary turned out to be enforced by the Go compiler as much as
// by discipline: stratagema's root package is a single flat `package
// main` (per go.mod), and Go does not allow importing a `package main`
// from anywhere, under any circumstances. So this package reuses nothing
// from the root package -- not ParseFacultyFile, not the Coordinator
// interface, not RemoteStore, not any domain type -- and instead
// reimplements the small, stable slice of each it actually needs:
// faculty.go (a trimmed Faculty parser, name+harness+body only) and
// client.go (a coordinatorClient speaking exactly two of Coordinator's
// HTTP routes). See both files' doc comments for the fuller reasoning,
// including why a "extract a shared importable package out of the root
// main package" alternative was considered and rejected for this pass:
// it would mean real surgery across the whole existing, already-tested
// root package for two HTTP calls' worth of reuse -- out of proportion to
// "the smallest real slice" this task was scoped as.
//
// The one load-bearing piece of actual logic here: docs/stratagema-
// classical-computing-analogies.md (in the sibling `boat` repo) names
// "exit codes vs. self-reported success" as Stratagema's sharpest honest
// gap -- every strategy_event today is self-attested by whichever
// identity chose to write it, with no equivalent of a Unix process's exit
// code, "the one signal in the entire model that isn't self-reported."
// This runner is a partial, real answer for exactly one case: the harness
// process it launches has a real exit code, supplied by the kernel, not
// by the harness lying about its own success. Exit 0 logs a
// step_completed event; non-zero logs a finding event honestly stating
// the failure and its exit code. It is still just one event kind's worth
// of ground truth, not a general "gate" primitive -- see the analogies
// doc's own explicit statement that building that general primitive is a
// bigger, separate decision, not attempted here.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Exit codes distinguish three genuinely different situations, per this
// task's own requirement that a coordinator-unreachable failure and a
// harness failure not be conflated:
//
//	0 = the harness exited 0, and every event logged cleanly.
//	1 = the harness itself failed (a real, non-zero exit code) -- honestly
//	    reported as a finding event. This runner did its job correctly;
//	    the thing it ran did not succeed.
//	2 = a runner-level failure: bad flags, an unparseable Faculty file, the
//	    coordinator unreachable or rejecting a request, or the harness
//	    process not even starting. None of these are a signal about the
//	    harness's own work.
const (
	exitOK            = 0
	exitHarnessFailed = 1
	exitRunnerFailed  = 2
	maxEventNoteBytes = 4000 // a few KB cap -- see buildCompletedNote/buildFailureNote
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stratagema-runner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	facultyPath := fs.String("faculty", "", "path to the Faculty file to run (required)")
	harnessCmd := fs.String("harness-cmd", "", `the literal shell command to invoke, e.g. "claude -p" or "codex exec" (required)`)
	dbURL := fs.String("db", "", "coordinator URL, e.g. http://localhost:7979 -- a running `stratagema serve` instance (required)")
	identity := fs.String("identity", "", "an existing identity ID to act as (required; the runner does not create identities)")
	strategyID := fs.String("strategy", "", "an existing strategy ID to log against (required; the runner does not create strategies)")
	task := fs.String("task", "", "short description of what this run is for, appended to the Faculty's prose as the actual prompt (required)")
	token := fs.String("token", "", "identity token (overrides STRATAGEMA_TOKEN) -- prefer the env var, matching the main stratagema CLI's own tokenFlag convention")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `stratagema-runner -- launches one agent harness for one Faculty against one strategy, reporting back through Stratagema's real Coordinator API.

usage: stratagema-runner -faculty=<path> -harness-cmd=<cmd> -db=<url> -identity=<id> -strategy=<id> -task=<text> [-token=<token>]`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitRunnerFailed
	}

	var missing []string
	for name, v := range map[string]string{
		"-faculty": *facultyPath, "-harness-cmd": *harnessCmd, "-db": *dbURL,
		"-identity": *identity, "-strategy": *strategyID, "-task": *task,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "stratagema-runner: missing required flag(s): %s\n", strings.Join(missing, ", "))
		return exitRunnerFailed
	}

	fac, err := parseFacultyFile(*facultyPath)
	if err != nil {
		fmt.Fprintf(stderr, "stratagema-runner: faculty: %v\n", err)
		return exitRunnerFailed
	}
	fmt.Fprintf(stdout, "stratagema-runner: faculty=%s harness=%s strategy=%s identity=%s\n", fac.Name, fac.Harness, *strategyID, *identity)

	client, err := newCoordinatorClient(*dbURL)
	if err != nil {
		fmt.Fprintf(stderr, "stratagema-runner: %v\n", err)
		return exitRunnerFailed
	}

	tok := resolveToken(*token)
	if err := client.verifyIdentity(*identity, tok); err != nil {
		fmt.Fprintf(stderr, "stratagema-runner: identity verification failed: %v\n", err)
		return exitRunnerFailed
	}

	prompt := buildPrompt(fac, *task)

	fmt.Fprintln(stdout, "stratagema-runner: logging step_started...")
	if _, err := client.logEvent(*strategyID, *identity, "step_started", buildStartedNote(fac, *task, *harnessCmd)); err != nil {
		fmt.Fprintf(stderr, "stratagema-runner: could not reach coordinator to log step_started: %v\n", err)
		return exitRunnerFailed
	}
	fmt.Fprintln(stdout, "stratagema-runner: step_started logged")

	fmt.Fprintf(stdout, "stratagema-runner: invoking harness: %s\n", *harnessCmd)
	result := runHarness(*harnessCmd, prompt)

	if result.startErr != nil {
		note := fmt.Sprintf("runner failed to invoke harness command %q: %v", *harnessCmd, result.startErr)
		fmt.Fprintf(stderr, "stratagema-runner: %s\n", note)
		if _, logErr := client.logEvent(*strategyID, *identity, "finding", note); logErr != nil {
			fmt.Fprintf(stderr, "stratagema-runner: (also failed to log a finding for this: %v)\n", logErr)
		} else {
			fmt.Fprintln(stdout, "stratagema-runner: finding logged (harness never started)")
		}
		return exitRunnerFailed
	}

	fmt.Fprintf(stdout, "stratagema-runner: harness exited with code %d\n", result.exitCode)

	if result.exitCode == 0 {
		note := buildCompletedNote(result.stdout)
		if _, err := client.logEvent(*strategyID, *identity, "step_completed", note); err != nil {
			fmt.Fprintf(stderr, "stratagema-runner: harness succeeded but could not reach coordinator to log step_completed: %v\n", err)
			return exitRunnerFailed
		}
		fmt.Fprintln(stdout, "stratagema-runner: step_completed logged")
		return exitOK
	}

	note := buildFailureNote(result.exitCode, result.stderr)
	if _, err := client.logEvent(*strategyID, *identity, "finding", note); err != nil {
		fmt.Fprintf(stderr, "stratagema-runner: harness failed AND could not reach coordinator to log the finding: %v\n", err)
		return exitRunnerFailed
	}
	fmt.Fprintln(stdout, "stratagema-runner: finding logged (harness failed)")
	return exitHarnessFailed
}

// resolveToken mirrors identity.go's own resolveToken precedence exactly:
// an explicit -token wins, else STRATAGEMA_TOKEN, else unprotected (empty).
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("STRATAGEMA_TOKEN")
}

// buildPrompt concatenates the Faculty's own prose with the operator's
// -task description as two clearly labeled sections, not one unlabeled
// blob -- so a harness (or a human reading a captured prompt later) can
// tell "who I'm supposed to be" from "what I'm supposed to do right now."
func buildPrompt(f *Faculty, task string) string {
	var b strings.Builder
	b.WriteString("# Faculty: ")
	b.WriteString(f.Name)
	b.WriteString("\n\n")
	b.WriteString(f.Body)
	b.WriteString("\n\n# Task\n\n")
	b.WriteString(task)
	b.WriteString("\n")
	return b.String()
}

func buildStartedNote(f *Faculty, task, harnessCmd string) string {
	return fmt.Sprintf("stratagema-runner starting faculty=%s harness-cmd=%q task=%q", f.Name, harnessCmd, task)
}

// truncateForNote caps b at maxEventNoteBytes -- strategy_events are read
// by humans and agents scanning a log, not a place to dump megabytes of
// raw harness output; a truncated note says so explicitly rather than
// silently cutting off.
func truncateForNote(b []byte) (text string, truncated bool) {
	s := strings.TrimSpace(string(b))
	if len(s) <= maxEventNoteBytes {
		return s, false
	}
	return s[:maxEventNoteBytes], true
}

func buildCompletedNote(stdout []byte) string {
	s, truncated := truncateForNote(stdout)
	if s == "" {
		s = "(harness produced no stdout output)"
	}
	if truncated {
		return fmt.Sprintf("harness exited 0. output (truncated to %d bytes):\n%s\n...[truncated]", maxEventNoteBytes, s)
	}
	return fmt.Sprintf("harness exited 0. output:\n%s", s)
}

func buildFailureNote(exitCode int, stderr []byte) string {
	s, truncated := truncateForNote(stderr)
	if s == "" {
		s = "(harness produced no stderr output)"
	}
	suffix := ""
	if truncated {
		suffix = fmt.Sprintf(" (truncated to %d bytes)", maxEventNoteBytes)
	}
	return fmt.Sprintf("harness failed: exit code %d. stderr%s:\n%s", exitCode, suffix, s)
}
