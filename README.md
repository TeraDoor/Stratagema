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

**Auth over the wire** is now two independent, both opt-in layers:

- *Per-identity token verification* (unchanged from before): a protected
  identity (`identity create -protect`) works the same way remotely as
  locally — `-token=`/`STRATAGEMA_TOKEN` is checked via `POST
  /identities/verify`, which reads the token from a standard
  `Authorization: Bearer <token>` header and returns a real 401 on a
  missing or wrong one. This is its own HTTP round trip, separate from the
  mutating call that follows (`lock acquire`, `strategy log`, ...) —
  mirroring the existing local shape, where a `cmd*` function calls
  `VerifyIdentityToken` as an explicit pre-check before acting, not folded
  into the action itself. That costs one extra request per mutating
  command in remote mode versus local mode's single in-process call — an
  accepted, honest tradeoff, not something this feature tries to optimize
  away by restructuring how commands work. This answers "which identity is
  this caller allowed to act as."
- *Server-wide access gate* (new): `stratagema serve -access-token=<secret>`
  (or `STRATAGEMA_SERVER_ACCESS_TOKEN`) requires every single request —
  every route, including the previously-open `GET /health`, `GET /locks`,
  and `GET /stream/interest` — to carry that value on a dedicated
  `X-Stratagema-Access-Token` header, checked with
  `crypto/subtle.ConstantTimeCompare` before the request reaches any
  handler, or it's rejected with a real 401. This answers a coarser
  question: "can this caller reach the server at all." It's a genuinely
  separate header from `Authorization: Bearer` — deliberately, since a
  request against a gated server acting as a protected identity carries
  both at once (one proves you may reach the server, the other proves you
  may act as that identity), and conflating them into one header would make
  that combination impossible to express. Point any command at a gated
  server the same way as `-token`: set `STRATAGEMA_SERVER_ACCESS_TOKEN` in
  the calling process's environment (read once when `-db=http(s)://...`
  constructs its `RemoteStore`), or leave it unset against an ungated
  server — zero observable difference from before this feature existed.

**TLS.** `stratagema serve` also accepts `-tls-cert=<path> -tls-key=<path>`: when
both are given, it listens with `http.ListenAndServeTLS` instead of plain
`http.ListenAndServe`, and the startup banner and route-table print the
real `https://` scheme it's actually serving. Giving only one of the two is
a real configuration error, rejected at startup with a specific message —
never silently ignored or guessed at. Leaving both unset is the unchanged
default: plain HTTP, exactly as before.

This is deliberately narrow: no cert generation, no rotation, no
ACME/Let's Encrypt integration — that's real, separate infrastructure this
project isn't taking on. `-tls-cert`/`-tls-key` is for a quick,
self-contained option (a self-signed cert for testing, or a cert already
provisioned by other means) — for a real hosted production deployment, the
more common and recommended path is still a TLS-terminating reverse proxy
in front of plain-HTTP `serve` (Caddy, nginx, Cloudflare Tunnel, a cloud
load balancer), not native TLS.

**Real HTTP status codes**, not a blanket 200/500: 400 for a malformed or
incomplete request, 401 for a failed identity-token verification, 404 for
an unknown id, 409 for a genuine conflict (e.g. `lock acquire` on a resource
someone else already holds), 500 for anything unexpected. `RemoteStore`
turns a non-2xx response back into a plain Go error carrying the server's
message — not a typed error a caller can `errors.Is` against, matching how
every existing `cmd*` function already just does `if err != nil { die(...)
}` today.

**Partially-closed gap, stated plainly:** every mutating call to the
underlying `Coordinator` methods (`AcquireLock` and friends) still takes no
token parameter of its own to check, so `/identities/verify` remains the
one and only place per-identity token verification is enforced — the same
way today's local CLI enforces it as a separate pre-check rather than
inside `Store`'s own methods, and nothing new stops a raw
`POST /locks/acquire` that never called `/identities/verify` first, the
same way nothing stops a Go program that imports this package's `Store`
type directly from skipping `VerifyIdentityToken` today. What *is* new: the
optional `-access-token`/TLS pair above close the two gaps that used to be
unconditional — an unauthenticated caller reading every lock, identity, and
strategy event log over the network with zero gate, and no way to encrypt
the wire without a separate reverse proxy. Both remain opt-in and
coarse-grained by design, not a full access-control system:
**not built here, deliberately** — rate limiting/DoS protection (a real,
separate infrastructure concern); any per-route or per-client access
policy beyond the one shared server-wide gate (the existing per-identity
token system already covers fine-grained "who can act as which identity" —
`-access-token` is a different, coarser layer: "can you reach the server at
all," not an ACL system); and cert management/rotation/ACME (see "TLS"
above). Put a `serve` instance behind something that provides what's still
missing (a reverse proxy, a VPN, an SSH tunnel, a rate limiter) before
exposing it beyond a trusted network.

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
- **The server-wide access gate (`-access-token`) is opt-in and
  intentionally coarse-grained.** It's one shared secret gating "can this
  caller reach the server at all" on every route, checked once by shared
  middleware before any handler runs — not a per-route or per-client ACL
  system, and not a replacement for the per-identity token system above
  ("who can act as which identity" stays that system's job). **Not built,
  deliberately:** rate limiting/DoS protection, and any finer-grained
  policy than the one shared token — both real, separate problems. A
  `serve` instance left ungated (the default) behaves exactly as it always
  has.
- **Native TLS (`-tls-cert`/`-tls-key`) has no cert lifecycle behind it.**
  No generation, no rotation, no ACME/Let's Encrypt integration — bring
  your own cert (a self-signed one for testing, or one provisioned by
  other means). For a real hosted deployment, a TLS-terminating reverse
  proxy in front of plain-HTTP `serve` (Caddy, nginx, Cloudflare Tunnel, a
  cloud load balancer) is still the recommended path; native TLS here is
  for a quick, self-contained option, not a replacement for that.
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
