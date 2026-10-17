# Architecture

Detailed architecture notes for schmeide. The one-page summary is [`ARCHITECTURE.md`](../../ARCHITECTURE.md) at
the repository root, and everything here expands on it. Diagrams are Mermaid, so GitHub renders them inline.

| Doc                                   | Covers                                                                                  |
| ------------------------------------- | --------------------------------------------------------------------------------------- |
| [System context](system-context.md)   | GitHub, the control node, runners, harnesses, NATS over TLS, what runs where            |
| [Control node](control-node.md)       | webhook listener, embedded NATS, streams, subjects, consumers, KV, registration, tokens |
| [Runner](runner.md)                   | job pipeline, trust, worktrees, brief, harness choice, gate, push, review, merge       |
| [Job flows](job-flows.md)             | sequence diagrams for the main paths and the failure paths                             |
| [Data](data.md)                       | JetStream streams and buckets, the work directory, trust files, config files, logs     |
| [Credentials](credentials.md)         | installation tokens, NATS tokens, secrets, session environment, trust boundaries       |
| [Build and tooling](tooling.md)       | commands, Makefile, lint config, CI workflows, release, test fakes, e2e journeys       |

These pages describe structure, which changes slowly. For what schmeide does feature by feature, the inventory is
[`docs/user-journeys/`](../user-journeys/README.md). For where files go and what the checks enforce, see the
repository's [`AGENTS.md`](../../AGENTS.md) and the [Build and tooling](tooling.md) page. When a diagram here disagrees with the
code, the code wins: fix the diagram.
