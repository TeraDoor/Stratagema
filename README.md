# Stratagema

A minimal system for coordinating multiple AI agents that share resources.

Two agents editing the same file, calling the same API, or working the same
task need exactly two primitives to not step on each other: a way to **claim**
a shared resource, and a way to be **told** when someone else changed it.
Stratagema is just that, deliberately, and nothing else.

The steering mechanism this is built around is called **intent transfer** —
the definition is still being worked out in the open, not a finished concept
being described after the fact.

## Status: early, work in progress

- **Resource locks** — designed, not yet implemented. An agent acquires a
  named lock before touching a shared resource; another agent asking for the
  same lock is told no, not left to find out the hard way.
- **Interest / event propagation** — implemented and dry-run tested. An
  agent declares interest in a resource; when another agent updates it, an
  event is created and can be pushed live over SSE to anyone subscribed.

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

## Who this is for

A solo developer running more than one agent against the same project who
wants those agents to not clobber each other's work, and to find out about
relevant changes without polling by hand.

## License

MIT — see [LICENSE](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
