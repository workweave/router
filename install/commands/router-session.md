---
description: Show the Weave Router session id for the current Claude Code session.
allowed-tools: Bash(echo:*), Bash(ls:*)
---

Report the Weave Router session id for this session. The router keys sessions on Claude Code's own session id, so:

1. Run `echo "$CLAUDE_CODE_SESSION_ID"`.
2. If that's empty, fall back to the most recently written transcript (this session is writing to it right now): run `ls -t ~/.claude/projects/*/*.jsonl | head -1` — the filename (without `.jsonl`) is the session id.

Then tell me the session id in one line, formatted as inline code so I can copy it.
