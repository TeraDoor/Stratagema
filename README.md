# Stratagema

A minimal system for coordinating multiple AI agents that share resources.

Two agents editing the same file, calling the same API, or working the same
task need exactly two primitives to not step on each other: a way to **claim**
a shared resource, and a way to be **told** when someone else changed it.
Stratagema is just that, deliberately, and nothing else.

The steering mechanism this is built around is called **intent transfer** —
the definition is still being worked out in the open, not a finished concept
being described after the fact.

## Status: early, both primitives implemented and dry-run tested

- **Resource locks** — `lock acquire/release/status/list`. One holder per
  named resource, fail-fast: asking for a held lock gets an immediate no
  (who holds it, since when), never a queue. Re-acquiring your own lock is
  idempotent; releasing someone else's requires `-force`, always logged.
- **Interest / event propagation** — `interest create/list/pause/resume/
  inbox/ack`. Subscribe to a resource by name; a denied acquire or a
  release notifies every active subscriber (an uncontested acquire stays
  quiet — nobody needs telling that nothing happened). Deliverable by
  polling `inbox`, or live over SSE via the optional `serve`.

Not yet done: packaged releases, a versioned `v1.0.0` tag. Build from
source for now.

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

## Known limitations

Found by an actual durability/edge-case pass, not theoretical — stated
here rather than left for someone to discover:

- **No identity authentication.** An `-identity=` value is a bare,
  unverified string everywhere. Any process that can reach the database
  can acquire, release, or force-release a lock under any identity,
  including one it doesn't "own." Fine for one trusted developer's own
  machine; a real gap before recommending shared, less-trusted, or
  adversarial use.
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
