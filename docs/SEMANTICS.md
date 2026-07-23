# Router semantics

> **Established 2026-07-23.** Canonical definitions for the Weave Router
> codebase and documentation. These terms replace the historical usage
> where "turn" often meant a single upstream API request.

## Terms

| Term | Definition | Notes |
|------|------------|-------|
| **Session** | The full conversation between a user and an agent. | Identified by `session_id`. Spans many rounds. |
| **Round** | One user-agent interaction within a session. | Typically one user turn followed by one agent turn. |
| **Turn** | A single user input or a single agent output within a round. | E.g. a user message (`U1`) or an agent response (`A1`). |
| **Action** | A single agent API request to an upstream model. | One `POST /v1/messages`, `POST /v1/chat/completions`, or `generateContent` call. This is the router's finest addressable routing grain. |
| **Step** | An action within a turn, addressed by its ordinal position. | An agent turn is made of one or more steps; each step is one action. `A1_S3` = agent turn 1, step 3 (its 3rd action). |

**Action vs. step.** An *action* and a *step* denote the same physical unit —
one upstream API request. "Action" names *what it is* (a single agent API
request, the routable grain); "step" names *where it sits* (its ordinal
position within a turn). Use "action" when talking about routing; use "step"
when addressing a specific request inside a turn.

## Hierarchy

```
Session
  └── Round (one user-agent interaction)
        └── Turn (one user input or one agent output)
              └── Step (one action within the turn = one upstream API request)
```

A session contains many rounds. Each round is typically one user turn plus one
agent turn. An agent turn is made of one or more **steps**, and each step is a
single **action** — one upstream API request that the router routes
independently.

## Notation

Turns are labeled by actor and index; steps and subagents nest after them with
`_`-separated segments.

| Label | Meaning |
|-------|---------|
| `U1` | User turn 1 |
| `A1` | Agent turn 1 |
| `A1_S3` | Agent turn 1, step 3 (its 3rd action) |
| `A2_S4_SA1_S1` | Agent turn 2, step 4, subagent 1, step 1 |
| `A2_S4_SA2_S1` | Agent turn 2, step 4, subagent 2, step 1 |

Segment prefixes: `U` = user turn, `A` = agent turn, `S` = step, `SA` =
subagent. When a step spawns subagents, each subagent carries its own steps
(`..._SA<n>_S<m>`), so the two subagents launched from `A2_S4` above are
`A2_S4_SA1_S1` and `A2_S4_SA2_S1`.

## What the router routes

The router makes a decision per **action** (i.e. per **step**): for every
inbound API request, it selects a model and provider. It does not route per
turn, per round, or per session, though it may use session-level state (e.g.
the session pin) to inform the per-action decision.

## Legacy naming

Some existing code and older documentation still uses **"turn"** to mean what
we now call an **action** / **step** (a single upstream API request). Notable
historical names:

- `internal/router/turntype` — classifies an **action** based on the kind of
  step it handles (e.g. `MainLoop`, `ToolResult`). A future rename to
  `actiontype` would align with these semantics.
- "per-turn routing" / "per-turn orchestration" in older docs means
  **per-action** routing.
- "turn loop" in `internal/proxy` means the **action loop** that processes
  one inbound API request.

New code, documentation, and telemetry should use the terms above. When
touching legacy docs, prefer updating them to the new terminology or adding a
note that they use the pre-2026-07-23 convention.
