# Context for picking this project back up

This repo has 21+ branches spanning a fast, adversarial build history
(2026-09-15 → present). Reading them in the wrong order, or reading all
of them when the task doesn't need it, wastes real time. This is a
verified reading order, not a guess — every branch/commit cited here was
checked against real `git log` output, not recalled from memory.

**How to use this**: read Tier 0 always. Read Tier 1 always — it's one
commit and it tells you what's deliberately *not* here. Stop at Tier 2
unless your task actually touches something in Tier 3/4. Tier 5 and 6
are this session's additions and the design-history repo, respectively.

## Tier 0 — orientation (always read first)

1. `CLAUDE.md` (this repo, root) — the manual-test push gate, the
   "verify, don't assume" rule, scope discipline pointer.
2. `GETTING_STARTED.md` — the real, runnable walkthrough (two identities
   racing for a lock), including the honest limits section ("a lock
   protects the metadata, not the file's content").
3. `README.md`'s "what this deliberately is not" section — before
   proposing anything that sounds like a UI, an audit/compliance layer,
   or a background orchestrator.
4. `git log --oneline main` — current state. As of this session: 25
   commits, ending at the four described in Tier 5.

## Tier 1 — the lean-core boundary (always read, it's one commit)

```
git show 6a6e52c
```

"Lean core: strip remote coordination, the access gate/TLS, and
stratagema-runner." `main` is not the full feature set — it's a
deliberate subtraction from a fully-tested superset (`dev`), keeping
Identity + Lock (leases, strategy-linkage) + Interest/Propagation +
Strategy + Tactic (renamed from Faculty this session), explicitly
excluding remote coordination, the network access gate/TLS, and the
runner. Knowing this one commit exists is the difference between
"rebuilding a feature that was deliberately cut for now" and "extending
the current lean core correctly."

## Tier 2 — capability → branch map (what's actually in `main`)

Each row: the capability as it exists in `main` today, the `feat-*`
branch that built it, and its paired `test-*` robustness pass. Both are
already on `origin` — read via `git log <branch>` or `git show`, no
checkout needed for most of them.

| Capability | Built in | Hardened in |
|---|---|---|
| Lock + Interest/Propagation (the original core) | `817f866` (base commit, predates the branch wave) | `test-lock-lease`, `test-strategy-interest` |
| Strategy + Faculty/Tactic (base) | `5244347` (base commit) | `test-faculty` (real path-traversal bug), `test-strategy-interest` |
| Opt-in lease/expiry on locks | `feat-lock-lease` | `test-lock-lease` (extreme-lease overflow, negative-lease bypass, both fixed) |
| Locks linked to strategies (`strategy_id`) | `feat-locks-strategies` | `test-lock-lease` |
| Strategy grouping (`group_name`) | `feat-strategy-grouping` | `test-strategy-interest` |
| Per-harness resource_usage logging | `feat-token-reporting` | — (still hand-typed; see Tier 5's roadmap note) |
| Opt-in identity token auth (`-protect`) | `feat-identity-auth` | `test-identity-cli` (silent trailing-flag drop, newline display, both fixed) |
| Durability / schema-mismatch characterization | — | `test-durability` (named the gap; **closed for real this session**, see Tier 5) |
| Faculty → Tactic rename | this session | — |

## Tier 3 — built, but deliberately not in `main` (read only if your task needs it)

These exist on `dev` and their own `feat-*`/`test-*` branches, fully
tested, but excluded by the Tier 1 cut. Don't assume they're available
in `main` just because they're in the repo.

- **Remote coordination** (`-db=http(s)://host:port`) — `feat-remote-coordinator`, `test-remote-auth`
- **Server-wide access gate + native TLS** — `feat-server-hardening`, `test-remote-auth`
- **`stratagema-runner`** (a separate binary that actually launches a harness) — `feat-runner`, `test-runner` (real shell-injection-resistance proof, bounded output capture)

If a task needs one of these, start from `dev`, not `main` — `dev` is
the fully-merged superset (231 tests as of `test-durability`'s merge,
2026-09-19).

## Tier 4 — cut, don't rebuild without reading why first

- `cut-external-signal` — `external_signal` as a strategy_event kind was
  schema'd, tried, and cut because it never got a real writer. If you're
  about to add a "record what happened after this shipped" mechanism,
  read this commit's message first; it's the same idea, and the reason
  it was cut (no real writer, not that the idea was wrong) is still
  unresolved — see Tier 5's roadmap.
- `cut-faculty-core` — a speculative lifecycle-stage field on Faculty
  (`core: plan|produce|verify|deliver`) was added, found to have zero
  real consumers, and cut. `ParseTactic` still silently ignores a
  leftover `core:` line by design (see `tactic_test.go`'s
  `TestParseTacticIgnoresLeftoverCoreLine`) — don't reintroduce the
  field without a real consumer already needing it.

## Tier 5 — this session's additions (2026-09-22)

Four real commits, on top of `6a6e52c`, now on `origin/main`:

1. `7eab483` — Faculty → Tactic rename (naming register reversal;
   `faculty.go`→`tactic.go`, CLI verb, env var, directory, all of it)
2. `d0a7a34` — fixed a usage-string bug: `strategy activate -h`/
   `interest pause -h`/`interest resume -h` printed the wrong subcommand
   name (built from the target *status*, not the verb typed)
3. `f275f83` — **closed the Tier 2 durability gap for real**: `migrate()`
   was `CREATE TABLE IF NOT EXISTS` only, a no-op against an
   already-existing older-shaped table. Added `addEvolvedColumns`, still
   no version table/migration framework, same design philosophy the
   original comment argued for, just made to actually hold
4. `99da61a` — `tactic create` now tells you at creation time that its
   body is a placeholder and where to edit it, instead of only saying so
   inside the file itself

**A real, durable Strategy record of this work exists locally, not in
this git history**: `~/stratagema/.stratagema/events.db` is gitignored
by design (runtime state, not source), and `strategy list`/`show`
against it only works on the machine that has it. It holds this
project's actual dogfood history — the real Sep-16 lock-coordination
episode wiring `strategy`/`faculty` into `main.go`'s dispatch, and this
session's `self-hosting-roadmap` strategy group.

**A frozen 2026-09-22 snapshot of that history is committed and
public**, in `~/boat/docs/examples/stratagema-dogfood-history/` — a
`reflection.md` narrative, `raw-events.md` (every row, verbatim, not
summarized), and the raw `events.db.snapshot-2026-09-22` file itself.
If you're an agent on a different machine, that's your substitute for
the live local file — current as of the date in its filename, not live.

**Open roadmap, logged in `self-hosting-roadmap`, not yet built**:
harness-enforced lock (still advisory-only), real per-invocation
`resource_usage` reporting (still hand-typed), a Schema executor (Schema
docs are hand-authored, nothing parses/dispatches them), a real
`external_signal` writer (see Tier 4), and repeated non-reused
comparative benchmark trials (the one real trial so far is n=1 vs n=1,
self-scored).

## Tier 6 — the design/vision layer (a separate repo: `~/boat`)

Stratagema's product code lives here; its design history, naming
decisions, and benchmark write-ups live in `~/boat/docs/stratagema-*.md`
and `~/boat/LEDGER.md` + `~/boat/ledger/`. Most relevant if you need the
*why* behind something in this repo, not the *what*:

- `stratagema-naming-and-references.md` — naming register, including the
  2026-09-22 reversal (military/defense-tech register now preferred over
  the earlier mind/consciousness one) that produced this session's
  Faculty → Tactic rename
- `stratagema-classical-computing-analogies.md` — works out that System 2
  (the cross-strategy Leader Faculty/Tactic) is the actual
  reconciliation-loop shape, hand-run by a human today, not an automated
  one
- `stratagema-comparison-benchmark-design.md` + `docs/examples/
  stratagema-benchmark-b1-baseline/` — the one real coordination-vs-no-
  coordination trial run so far
- `LEDGER.md` + `ledger/NNN-*.md` — numbered, append-only session history;
  read the *Current Status* section and the latest entry before anything
  else in this repo

Three artifacts from this session (private unless explicitly shared —
ask the operator for access if you're a different agent/session):
"Lock & Lease Trials," "Readiness Assessment," and "Compounding Memory"
— live scenario runs and a staged positioning draft, referenced from
`~/boat/ledger/` but hosted on claude.ai, not in either git repo.
