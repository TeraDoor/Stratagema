# Stratagema

A minimal system for coordinating multiple AI agents that share resources.

Two agents editing the same file, calling the same API, or working the same
task need exactly two primitives to not step on each other: a way to **claim**
a shared resource, and a way to be **told** when someone else changed it.
Stratagema is just that, deliberately, and nothing else.

The steering mechanism this is built around is called **intent transfer** —
the definition is still being worked out in the open, not a finished concept
being described after the fact.

## Status: early, four primitives real and tested

- **Resource locks** — `lock acquire/release/renew/status/list`. One holder
  per named resource, fail-fast: asking for a held lock gets an immediate
  no (who holds it, since when), never a queue. Re-acquiring your own lock
  is idempotent; releasing someone else's requires `-force`, always logged.
  Leases are opt-in (`-lease=<seconds>`, then `lock renew`): a lock
  acquired without one behaves exactly as before, but a leased lock whose
  holder stops renewing — a crashed agent, not a slow one — can be
  reclaimed by someone else without `-force`, logged as its own
  `reclaimed` event distinct from a forced override. Staleness is checked
  lazily wherever a lock is read, never by a background process —
  Kubernetes' Lease API is the closer precedent than a liveness probe,
  since it doesn't need a resident watcher either.
- **Interest / event propagation** — `interest create/list/pause/resume/
  inbox/ack`. Subscribe to a resource by name; a denied acquire or a
  release notifies every active subscriber (an uncontested acquire stays
  quiet — nobody needs telling that nothing happened). Deliverable by
  polling `inbox`, or live over SSE via the optional `serve`.
- **Strategy ledger** — `strategy create/list/show/log/log-usage/usage/
  activate/observe/close/next`. An append-only record of what agents
  actually found and decided while working — not locks-and-notifications,
  a durable log a human or another agent can read back later. Closing a
  strategy always logs a real final event, not just a status flip. `next`
  gives an agent resuming work a concise recap (events since the last
  completed step, plus every lock currently held system-wide) — it only
  reads and prints, never recommends or decides; that stays the calling
  agent's job. `log-usage` records a `resource_usage` event in a fixed,
  parseable shape (`harness=... tokens=... [cost=...]`) instead of
  freeform prose, and `usage` reads it back with a per-harness total —
  manual for now (real per-invocation reporting from a harness is a
  bigger, harness-dependent lift, not built here), but comparable once it
  exists.
- **Tactic tooling** — `tactic create/list/show`. A Tactic is an
  agent-role definition (a markdown file: settings up top, behavior in
  prose below). The format was already usable by hand; this is scaffolding,
  listing, and viewing them without hand-editing files directly.

Not yet done: a Planner that proposes a strategy from a one-line intent, and
a way for a strategy to stay open and keep collecting findings after
whatever it built has shipped. Packaged releases are wired up — pushing a
`v*` tag runs `.github/workflows/release.yml`, which cross-compiles
linux/darwin/windows binaries and attaches checksummed archives to a
GitHub Release (see [Releases](#releases) below) — but no real tag has
been pushed through it yet, so that path is built and locally verified,
not yet proven end-to-end on GitHub's infrastructure. Build from source
until a `v1.0.0` (or earlier) tag actually goes out.

**New here?** [`GETTING_STARTED.md`](GETTING_STARTED.md) walks through the
one property this project has to get right — two agents, one resource,
exactly one winner — by hand from a clean clone, then proves it under real
concurrent processes. The Quickstart below is the fuller flow.

## Quickstart

```
go build -o bin/stratagema .

DB=./demo.db
BIN=./bin/stratagema

alpha=$($BIN identity create -db=$DB -label=agent-alpha | head -1 | cut -d' ' -f2)
beta=$($BIN identity create -db=$DB -label=agent-beta  | head -1 | cut -d' ' -f2)

$BIN interest create -db=$DB -identity=$beta -resource=shared-config

$BIN lock acquire -db=$DB -resource=shared-config -identity=$alpha -note="editing pool size"
$BIN lock acquire -db=$DB -resource=shared-config -identity=$beta  # denied — alpha holds it
$BIN interest inbox -db=$DB -identity=$beta                        # sees the denial

$BIN lock release -db=$DB -resource=shared-config -identity=$alpha -note="pool size bumped, safe to read"
$BIN interest inbox -db=$DB -identity=$beta                        # sees the release + note
$BIN lock acquire -db=$DB -resource=shared-config -identity=$beta  # now succeeds
```

For live push instead of polling `inbox`, run `stratagema serve -db=$DB`
in another terminal and `curl -sN "http://localhost:7979/stream/interest?identity=$beta"`.

`serve` is read-only from the outside: `GET /health`, `GET /locks` (a JSON
snapshot of active locks), and `GET /stream/interest?identity=<id>` (the SSE
feed above). There is no way to acquire a lock, create an identity, or log a
strategy event over the wire — every mutating command still talks to the
local SQLite file named by `-db` directly. `stratagema serve`'s own startup
output prints the exact three routes.

## What this deliberately is not

- **No UI.** CLI only, for now.
- **No audit/compliance layer.** No hash-chained log, no export formats, no
  procedure registry, no zero-trust-agent machinery.
- **No forever-running orchestrator.** Nothing schedules or drives agents in
  the background. The optional push server is a short-lived process you
  start when you want live event delivery and stop when you don't — it does
  not run your agents for you.
- **No harness lock-in.** Talk to it from Claude Code, Codex, Pi, or a bare
  curl script from a shell — it does not care what is driving the agent on
  either end.
- **No remote coordination.** Every command always opens a local SQLite
  file named by `-db`; there is no way to point one process's CLI at
  another machine's database over the network. `serve`'s three routes
  (above) are the only network surface this branch has, and they're
  read-only.

## Releases

`stratagema version` reports the real tag it was built from, stamped at
build time — not a hardcoded string:

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o bin/stratagema .
```

A plain `go build -o bin/stratagema .` (no `-ldflags`) still works exactly
as before and reports `stratagema 0.0.0-dev` — the local dev loop is
unchanged.

Pushing a tag matching `v*` runs `.github/workflows/release.yml`: it
cross-compiles for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64,
and windows/amd64 (no cgo anywhere in this module, so plain `GOOS`/`GOARCH`
builds are enough — no cross-compiler toolchain needed), packages each as
a `.tar.gz` (`.zip` on Windows) with the binary plus `README.md`/`LICENSE`,
computes a `sha256` checksum per archive, and publishes all of it to a
GitHub Release on that tag.

To test the cross-compilation matrix locally before trusting a tag push,
without any extra tooling:

```
for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  goos=${pair%/*}; goarch=${pair#*/}
  out="/tmp/stratagema-$goos-$goarch"; [ "$goos" = windows ] && out="$out.exe"
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -ldflags "-X main.version=test" -o "$out" .
done
```

Each invocation should exit 0 and leave a binary at `$out`.

## Known limitations

Found by an actual durability/edge-case pass, not theoretical — stated
here rather than left for someone to discover:

- **Identity authentication is opt-in, per identity, not a blanket
  requirement.** `identity create -protect` generates a real random
  secret (32 bytes from `crypto/rand`), prints it exactly once, and
  stores only its `sha256` hash. From then on, every identity-scoped
  mutating command for that identity — `lock acquire/renew/release`,
  `interest create`, `strategy log/log-usage/close` — requires a valid
  token (`STRATAGEMA_TOKEN` env var, or `-token=` for scripting) via a
  single shared, constant-time-compared verification path, or it's
  rejected cleanly. `interest inbox` is included too: reading a
  protected identity's own inbox can leak another identity's lock notes
  and activity pattern, so it's treated as the same concern even though
  it's a read, not a mutation. An identity created the plain way (no
  `-protect`, everything before this feature, and the default today)
  behaves with zero observable difference — still just a bare,
  unverified label, exactly as before. **Not built, deliberately:**
  token rotation, revocation, un-protecting an identity after creation,
  or any session/expiry mechanism for the token itself — protection is
  permanent once set, and the one token issued at creation is the only
  one that will ever work. Fine for one trusted developer's own machine
  even before touching this; a real step toward shared, less-trusted, or
  adversarial use once identities that need it are actually protected —
  not a complete access-control system on its own.
- **SSE reconnect does a full replay, not a resume.** `/stream/interest`
  has no durable cursor across a `serve` restart — a client that
  reconnects gets everything again from the start, not just what it
  missed. Fine for the CLI's own polling fallback (`interest inbox`
  isn't affected); a gap if you're building something that assumes
  exactly-once live delivery.
- **WAL mode's safety over a network filesystem is unverified, and
  SQLite's own documentation warns against it.** Putting the database
  file on NFS or similar isn't tested and isn't recommended — keep it on
  local disk, one machine, until this is specifically addressed.
- **No schema-version check.** Two binary versions with an incompatible
  schema sharing one database file isn't detected or prevented — keep
  the binary in sync across every process touching the same database.
  Empirically confirmed (not just architecturally assumed) that the two
  mismatch directions fail differently, and neither is silent
  corruption: an **older binary opening a database a newer binary
  already wrote grown columns into** (e.g. `lease_seconds`/`strategy_id`
  on `locks`, `token_hash` on `identities`) opens and keeps working —
  `migrate()`'s `CREATE TABLE IF NOT EXISTS` never touches an existing
  table, so the old binary's own, smaller queries never even reference
  the columns it doesn't know about — but it is blind to any semantics
  those columns carry: it cannot see an expired lease and will refuse
  forever to reclaim a resource a current binary would correctly free.
  A **newer binary opening an older, smaller-schema database** fails
  loudly instead: every current `INSERT`/`SELECT` in this codebase
  names the grown columns unconditionally, so the very first read or
  write against the old schema returns a plain `no such column`/`has no
  column named` SQL error and a clean non-zero exit — never a partial
  write, a crash, or quietly-wrong data. Net: downgrading a binary
  against a newer database is the dangerous direction (silent staleness
  on lease-aware resources); upgrading is the safe one (loud, immediate
  failure). Still not detected or prevented either way — the fix
  remains "keep the binary in sync," a real, separate feature (schema
  versioning) this project deliberately doesn't have.

## Who this is for

A solo developer running more than one agent against the same project who
wants those agents to not clobber each other's work, and to find out about
relevant changes without polling by hand — on one machine, one local
database file. Coordinating across machines needs the remote-coordination
branch of this project; this branch deliberately doesn't have that.

## License

MIT — see [LICENSE](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
