# Project instructions (Stratagema)

**Picking this project back up, or new to it?** Read `CONTEXT.md` first
— a verified, tiered guide to which of this repo's 21+ branches actually
need reading for a given task, and which are safe to skip.

## Every project decision goes through Stratagema — MANDATORY, no exceptions

**Rule, stated as strictly as it can be stated**: any development, sales,
marketing, or product decision about this project — not just a code
change — is created and logged through Stratagema itself, not made and
left only in chat, a doc, or someone's head. If the mechanism a decision
needs doesn't exist in Stratagema yet, building that mechanism is itself
the next thing done through Stratagema — not worked around, not done
informally "for now."

This applies to every human and every agent working on this project, no
seniority exception, no "just this once."

**Concretely, before doing the work:**
1. `identity create` once, if you don't already have one for this
   project.
2. `strategy create -thesis="..."` for the bounded piece of work — a
   feature, a positioning call, a pricing decision, a naming reversal,
   anything. The thesis is the decision or question, stated plainly.
3. `strategy activate`.
4. `lock acquire -strategy=<id>` on any file you're about to touch —
   code, docs, whatever the decision actually changes.

**While doing it:**
5. `strategy log -kind=decision|finding|step_started|step_completed`
   for anything decided or learned, logged when it happens, not
   reconstructed afterward. This applies to non-code decisions exactly
   the same as a code change — a positioning call or a scope cut gets a
   `decision` event, not just a mention in a chat transcript somewhere
   else.

**When done:**
6. `lock release`, then `strategy close -outcome="..."` — a real
   reflection, every time, closing what the thesis actually asked.

**Said honestly, not oversold**: today this is a discipline rule, not a
mechanical guarantee — the same admission `MANUAL_TEST.md` already makes
about itself, two sections down. `feat-harness-lock-hook` (built this
session, not yet merged to `main`) makes *file-edit* enforcement real
once it's actually wired up as a live `PreToolUse` hook — but that only
covers code being written, not a sales call or a pricing decision made
in conversation and never logged. Nothing here mechanically stops
someone from violating this rule the way nothing mechanically stops
`git push --no-verify`. It's binding because it's stated as mandatory
here, not because it's currently impossible to skip — closing that gap
for good is itself a decision that belongs on the self-hosting-roadmap
Strategy, logged through Stratagema, not solved by writing a stronger
sentence in this file.

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
