# Getting started

This walks through the one thing Stratagema has to be able to do, by hand,
from a clean clone — nothing else. If you want the fuller flow (a
subscriber finding out about a change over live SSE push, `-force`
release, etc.), see the [README Quickstart](README.md#quickstart) after
this.

## The one flow

**Two agents, one resource: exactly one of them may hold it at a time, and
the other is told no, cleanly, immediately.** Everything else in this
project — interest, propagation, `serve` — exists to make what happens
*after* that no more useful. If this one property doesn't hold, nothing
else here matters. It's proven under real concurrent processes in
`e2e_test.go`; this section is the same property, walked through by hand.

## 1. Clone and build

```
git clone https://github.com/TeraDoor/Stratagema.git
cd Stratagema
go build -o bin/stratagema .
git config core.hooksPath .githooks
```

Requires Go 1.23+. No other setup — no config file, no external service,
no database server to start. `bin/stratagema` is the whole thing.

That last line enables this repo's one piece of local process: `git push`
is blocked while `MANUAL_TEST.md` has an unresolved item — see that file
for why. It's a one-time, per-clone opt-in; git doesn't wire up a
repo's committed hooks on its own.

## 2. Create two identities

A real deployment would be two different agent processes (Claude Code,
Codex, Pi, anything) each running their own `identity create` once. For
this walkthrough, run both from one shell:

```
$ ./bin/stratagema identity create -db=./demo.db -label=agent-alpha
created ident-a1b2c3d4e5f6
  label: agent-alpha

$ ./bin/stratagema identity create -db=./demo.db -label=agent-beta
created ident-f6e5d4c3b2a1
  label: agent-beta
```

Your IDs will be different (they're random) — copy them for the next
steps, or export them:

```
alpha=ident-a1b2c3d4e5f6   # use your own printed value
beta=ident-f6e5d4c3b2a1    # use your own printed value
```

Everything after this uses `-db=./demo.db` throughout — one shared SQLite
file both identities act against, exactly as two real agent processes
sharing one project would.

## 3. Alpha claims the resource

```
$ ./bin/stratagema lock acquire -db=./demo.db -resource=shared-config -identity=$alpha -note="editing pool size"
acquired shared-config
  holder: ident-a1b2c3d4e5f6
  since:  2026-09-15T20:45:07Z
```

Exit code `0`. Nothing else happened — no notification went anywhere,
because nobody is subscribed yet and even if they were, an uncontested
claim isn't news.

## 4. Beta tries to claim the same resource

```
$ ./bin/stratagema lock acquire -db=./demo.db -resource=shared-config -identity=$beta
stratagema: lock acquire: resource is locked by another identity: ident-a1b2c3d4e5f6 holds "shared-config" since 2026-09-15T20:45:07Z
$ echo $?
1
```

This is the property. Beta is told no, immediately, with who holds it and
since when — not left to find out by clobbering alpha's work.

## 5. Alpha releases, beta claims cleanly

```
$ ./bin/stratagema lock release -db=./demo.db -resource=shared-config -identity=$alpha -note="pool size bumped, safe to read"
released shared-config

$ ./bin/stratagema lock acquire -db=./demo.db -resource=shared-config -identity=$beta
acquired shared-config
  holder: ident-f6e5d4c3b2a1
  since:  2026-09-15T20:45:19Z
$ echo $?
0
```

That's the whole minimal flow. Nothing here required `interest`, `serve`,
or any background process — just two identities and a resource name,
which is deliberately the entire surface area this property needs.

## Proving it under real concurrency, not just by hand

Doing this manually, one command after another, can't actually create a
race — your two hands aren't fast enough, and that's fine for a
walkthrough. The property that matters is what happens when two *real,
simultaneous* processes hit the same resource at once. That's exactly
what `e2e_test.go` does — it builds the real binary and launches a dozen
real OS processes at the same instant, all racing for one lock, then
checks that exactly one wins and every loser got the clean denial message
above, not a raw database error:

```
go test -run TestConcurrentAcquireHasExactlyOneWinner -v ./...
```

Run it a few times in a row (`-count=10`) if you want to see it hold up
repeatedly rather than trust one lucky pass.

## What's next

- The fuller flow — a subscriber finding out about a release *without*
  polling, over live SSE push — is the [README Quickstart](README.md#quickstart).
- `lock status` / `lock list` to inspect what's currently held.
- `-force` on `lock release` for breaking a stuck lock (see the README's
  "Status" section for why nothing expires locks automatically).
