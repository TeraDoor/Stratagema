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
- **Faculty tooling** — `faculty create/list/show`. A Faculty is an
  agent-role definition (a markdown file: settings up top, behavior in
  prose below). The format was already usable by hand; this is scaffolding,
  listing, and viewing them without hand-editing files directly.
- **Remote coordination** — every command's `-db` flag also accepts
  `http://host:port`/`https://host:port`, talking to a running `stratagema
  serve` over HTTP/JSON instead of opening a local SQLite file. This is
  what makes `serve` an actual hosted coordinator two agents on two
  different machines can share, not just a read-only dashboard over one
  process's local file. See "Remote mode" below.

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

## Remote mode: coordinating across machines

Every command's `-db` flag accepts a URL (`http://` or `https://`), not just
a local file path — this is what makes `serve` an actual hosted coordinator
instead of a read-only dashboard. Run `stratagema serve -db=$DB` on one
machine, then point any other process at it exactly like a local file:

```
# machine/process A: the coordinator
stratagema serve -db=./events.db -port=7979

# machine/process B (or a second terminal on the same one)
stratagema identity create -db=http://coordinator-host:7979 -label=agent-remote
stratagema lock acquire    -db=http://coordinator-host:7979 -resource=shared-config -identity=$id -note=editing
stratagema strategy create -db=http://coordinator-host:7979 -name=... -thesis=...
```

Every subcommand works identically either way — `-db=./file.db` and
`-db=http://host:port` are interchangeable everywhere a database path is
accepted, because `openStore` (`store.go`) is the single place that decides
which one a given value means; nothing downstream of it knows or cares.
Internally this is `RemoteStore` (`remote.go`), a second implementation of
the same `Coordinator` interface `*Store` already satisfies, talking
HTTP/JSON to the routes `serve` exposes (`serve.go`'s `newMux`) — `stratagema
serve`'s own startup output prints the full route table.

**Auth over the wire.** A protected identity (`identity create -protect`)
works the same way remotely as locally: `-token=`/`STRATAGEMA_TOKEN` is
checked via `POST /identities/verify`, which reads the token from a
standard `Authorization: Bearer <token>` header and returns a real 401 on a
missing or wrong one. This is its own HTTP round trip, separate from the
mutating call that follows (`lock acquire`, `strategy log`, ...) — mirroring
the existing local shape, where a `cmd*` function calls
`VerifyIdentityToken` as an explicit pre-check before acting, not folded
into the action itself. That costs one extra request per mutating command
in remote mode versus local mode's single in-process call — an accepted,
honest tradeoff, not something this feature tries to optimize away by
restructuring how commands work.

**Real HTTP status codes**, not a blanket 200/500: 400 for a malformed or
incomplete request, 401 for a failed identity-token verification, 404 for
an unknown id, 409 for a genuine conflict (e.g. `lock acquire` on a resource
someone else already holds), 500 for anything unexpected. `RemoteStore`
turns a non-2xx response back into a plain Go error carrying the server's
message — not a typed error a caller can `errors.Is` against, matching how
every existing `cmd*` function already just does `if err != nil { die(...)
}` today.

**Known gap, stated plainly, not built here:** the new write/read endpoints
have no network-level access control beyond identity-token verification on
the specific mutating calls that already checked it locally
(`AcquireLock`/`ReleaseLock`/`RenewLock`, `CreateInterest`, `strategy
log`/`log-usage`/`close`) — and even those endpoints don't themselves
re-verify a token on the mutating request itself, because the underlying
`Coordinator` methods (`AcquireLock` and friends) take no token parameter
to check; `/identities/verify` is the one and only enforcement point, the
same way today's local CLI enforces it as a separate pre-check rather than
inside `Store`'s own methods. A caller with direct network access to a
running `serve` has exactly the capability a caller with direct access to
the local `.db` file already has today: nothing stops a raw request to
`POST /locks/acquire` that never called `/identities/verify` first, the
same way nothing stops a Go program that imports this package's `Store`
type directly from skipping `VerifyIdentityToken` today. There is no
TLS, no general request authentication, and no rate-limiting anywhere in
this surface — the existing unauthenticated `GET /locks` and `GET
/stream/interest` were already this permissive before this feature
existed, and every new route matches that same posture rather than
inventing a stricter one inconsistently. A genuinely "hosted, safe by
default" server is a real, separate, bigger problem — TLS termination,
network-wide authentication, rate-limiting — not attempted here. Put a
`serve` instance behind something that provides that (a reverse proxy,
a VPN, an SSH tunnel) before exposing it beyond a trusted network.

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

## Who this is for

A solo developer running more than one agent against the same project who
wants those agents to not clobber each other's work, and to find out about
relevant changes without polling by hand.

## License

MIT — see [LICENSE](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
