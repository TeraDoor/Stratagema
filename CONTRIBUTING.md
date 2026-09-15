# Contributing

Stratagema is early and small on purpose. That changes how contributing
works here compared to a mature project.

## Before writing code

For anything that touches the core model — the lock primitive, the interest/
propagation matching rules, the "intent transfer" concept itself — open an
issue first and describe the problem you're solving. These pieces are still
being actively designed; a PR that lands before the shape is agreed on is
likely to be asked to change shape later, which wastes your time more than
mine.

Bug fixes, docs, tests, and anything scoped to a single file don't need this
— just open a PR.

## Making a change

1. Fork and branch from `main`.
2. Keep the change scoped to one thing. Small PRs get reviewed faster and
   are easier to reason about in a project this size.
3. Before opening the PR:
   - `go build ./...`
   - `go vet ./...`
   - `go test ./...`
4. Write a commit message that explains *why*, not just *what* — the diff
   already shows what changed.

## The PR gate — no exceptions, including for the maintainer

Every PR description starts from a template with two placeholder markers:
one under "how I tested it manually," one under "reviewed." CI fails the
PR — blocking merge — until both are gone, which only happens by actually
replacing the placeholder text with a real description of what you ran by
hand and confirming you re-read the full diff. `go test` passing is
necessary but not sufficient here on purpose: automated tests only check
what someone already thought to assert; this project's one real
correctness bug so far (`e2e_test.go`'s concurrent-acquire race) was found
by someone actually running the thing, not by a green test suite.

This applies to `main` directly, too — there's no direct push, and no
admin bypass on the required check. If you don't do the two things above,
you can't merge, and neither can I.

## Code style

Plain Go. No framework, no dependency added without a real reason — this
project's whole pitch is "minimal," and that has to be true of the
dependency list too, not just the feature list.

## Design decisions

There's no committee yet — it's one maintainer making calls, in the open,
in issues. If you disagree with a decision, say so on the issue; a good
argument can change it. Silence is not agreement, but an unaddressed
objection after a decision ships isn't grounds to relitigate it later either.

## Reporting a bug

Open an issue with: what you ran, what you expected, what happened instead,
and your OS/Go version. A minimal repro is worth more than a long
description.
