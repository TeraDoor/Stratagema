# Manual test gate

This file is a push gate, not documentation. `.githooks/pre-push` blocks
`git push` while any line below starts with `- [ ]` — the only way past
it is meant to be running the thing by hand and then deleting that line
yourself.

**Honestly: this is a local git hook, so it's a discipline aid, not a
hard boundary.** `git push --no-verify` skips it — confirmed directly,
not assumed — and always will, because that's how client-side git hooks
work everywhere, not a gap specific to this one. It exists to make
skipping the manual test a deliberate, visible act (typing
`--no-verify`, or deleting a line you didn't actually verify) instead of
an accidental one, not to make skipping it impossible. A real hard
boundary would need server-side enforcement (a pre-receive hook, or
GitHub branch protection once this repo has a GitHub remote) — out of
scope for a local-only setup.

Enable the hook once per clone (it's committed here, but git doesn't
auto-install hooks from the repo — this is the one-time opt-in):

    git config core.hooksPath .githooks

## Pending

(nothing pending right now)

## How this is meant to work

Whenever you add or change functionality, add a line under "Pending"
before you're done, describing exactly what needs to be run by hand and
what "it worked" looks like — specific enough that doing it is
unambiguous:

```
- [ ] `stratagema lock acquire` then a second `acquire` from a different
      identity on the same resource — confirm the second one is denied
      with the clean message, not a raw error.
```

Then actually do that, by hand, and delete the line. If you can't delete
it honestly, it isn't done.

## Why this file, and why local instead of CI

`.github/workflows/ci.yml` already runs `go build`/`go vet`/`go test` on
every push — that catches what an assertion already thought to check.
This file exists for the other half: a human actually running the new
behavior, the same discipline that caught `e2e_test.go`'s real
concurrency bug in the first place — a green test suite didn't find it;
someone deliberately going and testing the real property did. Kept local
and git-hook-enforced, not a GitHub PR check, so it works before this
repo is ever pushed anywhere and doesn't depend on GitHub at all.
