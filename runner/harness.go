package main

import (
	"bytes"
	"errors"
	"os/exec"
)

// harnessResult is everything runHarness actually observed about one
// invocation: the real exit code (the load-bearing signal -- see main.go's
// package doc comment), and the captured output used to build the
// step_completed/finding note.
type harnessResult struct {
	exitCode int
	stdout   []byte
	stderr   []byte
	// startErr is non-nil only when the harness process itself could not
	// be started at all (e.g. no working shell on this machine) -- a
	// runner-level failure distinct from the harness running and choosing
	// to exit non-zero, which is a normal, honestly-reported result, not a
	// failure of this tool. See main.go's run() for how the two are told
	// apart in the final exit code.
	startErr error
}

// runHarness invokes harnessCmd -- the operator-supplied shell command
// line, e.g. "claude -p" or "codex exec" -- through `sh -c`, so any
// quoting, pipes, or env-var references the operator wrote work exactly
// as they would typed directly into a shell, then appends prompt as one
// literal trailing positional argument via sh's own "$@" expansion. This
// is the standard `sh -c 'cmd "$@"' sh arg1 arg2...` idiom: prompt is
// never interpolated into the command string itself, so quote/backtick/$
// characters inside the prompt cannot be misread as shell syntax or
// escape into a second command -- it arrives as a single argv entry, the
// same way os/exec would pass any other argument.
//
// Trailing-argument was chosen over stdin because it matches the one
// concrete example this task was scoped against (`claude -p "<prompt>"`
// takes its prompt as an argument) and codex exec accepts a trailing
// positional prompt the same way; stdin is a real alternative some
// harnesses prefer, but a single, consistent mechanism is simpler for
// this first slice and is explicitly left as future scope (see README).
func runHarness(harnessCmd, prompt string) harnessResult {
	cmd := exec.Command("sh", "-c", harnessCmd+` "$@"`, "sh", prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := harnessResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if err == nil {
		res.exitCode = 0
		return res
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A real exit code from the harness process itself -- including
		// sh's own 127 (command not found) or 126 (not executable) when
		// harnessCmd names something that doesn't exist, which is a
		// legitimate, honestly-reportable harness failure, not a runner
		// bug.
		res.exitCode = exitErr.ExitCode()
		return res
	}

	// Could not even start/run the process (e.g. no `sh` on this
	// machine) -- a failure of this tool, not a signal about the harness.
	res.startErr = err
	res.exitCode = -1
	return res
}
