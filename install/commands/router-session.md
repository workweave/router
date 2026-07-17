---
description: Print the session id that correlates this session in router telemetry and logs.
allowed-tools: Bash(echo:*)
---

Print the session id used by the Weave Router to correlate this session's
telemetry, logs, and feedback submissions. Also works as a transcript
filename key for manual lookups in the router dashboard.

The router carries this id as `X-Claude-Code-Session-Id` on every request and
stores it in `model_router_users.SessionID` for analytics joins.

1. Run `echo "$CLAUDE_CODE_SESSION_ID"`.
2. If empty, the transcript filename under `~/.claude/projects/` for this
   session also carries the id — check the most recently written file.

Then tell me the session id in one line, formatted as inline code so I can copy it.
