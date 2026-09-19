package main

import (
	"bytes"
	"errors"
	"os/exec"
)

// maxCapturedOutputBytes bounds how much of a harness's stdout/stderr this
// runner will ever hold in memory. Before this cap existed, cmd.Stdout and
// cmd.Stderr pointed straight at a plain, unbounded bytes.Buffer:
// truncateForNote (main.go) only trims the *note* text built afterward, so
// a harness that actually wrote many MB of output was fully read into
// memory first -- a real unbounded-memory read, not just an unbounded
// logged note. Capped well above maxEventNoteBytes so the note text this
// produces is identical to the old unbounded behavior in every case that
// matters: truncateForNote never looks past maxEventNoteBytes anyway, so
// bytes beyond this cap were always going to be discarded by the time a
// note gets built -- they just used to sit in memory first.
const maxCapturedOutputBytes = maxEventNoteBytes * 4

// boundedWriter accepts up to limit bytes and silently drops the rest,
// always reporting a full write to its caller. os/exec's own stdout/stderr
// copy goroutine treats a write error as a reason to stop copying (and can
// trip a SIGPIPE on the child's side) -- a capture buffer that's simply
// full should never cause that.
type boundedWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) < room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

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
	stdout := &boundedWriter{limit: maxCapturedOutputBytes}
	stderr := &boundedWriter{limit: maxCapturedOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	res := harnessResult{stdout: stdout.buf.Bytes(), stderr: stderr.buf.Bytes()}
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
