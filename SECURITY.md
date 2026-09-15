# Security Policy

Stratagema coordinates multiple agents around shared resources — a bug in
lock granting or event delivery (e.g. a lock that can be bypassed, or a
propagation delivered to the wrong identity) is a security issue, not just
a correctness one, even though the project has no network-facing auth model
yet.

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Use GitHub's private **"Report a vulnerability"** flow on this repository
(Security tab → Report a vulnerability). That's the only channel this
project uses for security reports right now.

Include what you found, how to reproduce it, and what you think the impact
is. Expect an acknowledgment before a fix — this is a solo-maintained
project, response time will vary.

## Supported versions

Pre-1.0: only the latest commit on `main` is supported. No backported
security fixes to older tags yet.
