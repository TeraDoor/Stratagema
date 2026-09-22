# Project instructions (Stratagema)

**Picking this project back up, or new to it?** Read `CONTEXT.md` first
— a verified, tiered guide to which of this repo's 21+ branches actually
need reading for a given task, and which are safe to skip.

## Manual-test gate — MANDATORY, no exceptions

`MANUAL_TEST.md` (root) is a local, git-hook-enforced push gate — read it
before pushing anything, not just once.

1. Whenever you add or change functionality, append one line under its
   "Pending" section describing exactly what needs to be run by hand and
   what success looks like — specific enough that doing it is
   unambiguous, not "test the new feature."
2. Before pushing, actually do that manual test yourself, then delete the
   line. Never delete a line for something you didn't actually run.
3. `git push` is blocked locally (`.githooks/pre-push`) while any
   unresolved (`- [ ]`) line remains under "Pending." This applies to
   every push, including the maintainer's own — see `MANUAL_TEST.md`'s
   own "why" section, and its honest note that `--no-verify` bypasses it
   (a local hook always can be bypassed; that's a property of git hooks
   generally, not a gap in this one).
4. Hook setup is per-clone, not automatic:
   `git config core.hooksPath .githooks`. If a fresh clone's push isn't
   being gated, this is almost certainly why — check it before assuming
   the hook is broken.

## Verify, don't assume

This project's one real bug so far (`e2e_test.go`'s concurrent-acquire
race, plus a `SQLITE_BUSY` DSN parameter that looked right but wasn't
taking effect) was caught by actually running things under real
conditions, not by reasoning about the code. Before claiming a fix works,
a doc's steps are accurate, or a test proves what it claims to prove: run
it for real and look at the actual output. This applies doubly to
anything touching `lock.go`'s concurrency behavior.

## Scope discipline

See the README's "what this deliberately is not" section before adding
anything: no UI, no audit/compliance layer, no background orchestrator
beyond the optional `serve`. A PR or change that reintroduces one of
these needs a real reason stated up front, not just "it would be useful."
