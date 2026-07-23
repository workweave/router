<!--
Thanks for contributing to router! Fill this out so we can review quickly.
PRs that skip the checklist or leave testing blank will be sent back — not because we're strict for its own sake, but because an untested change costs more to review than to write.
-->

## What & why

<!-- What does this change, and what problem does it solve? Link the issue it closes: "Closes #123". -->

## How it was tested

<!-- Required. What did you actually run? Paste the command(s) and the result. "make check passes" is the floor, not the whole answer — if you fixed a bug, show the behavior before/after. -->

## AI assistance

<!--
If you used an agent (Claude Code, Cursor, etc.) to write any of this, tell us how and paste the prompt(s) you used.
This is not a gotcha — it genuinely helps us review faster and trust the change. Undisclosed slop is the thing we send back.
-->

- [ ] This PR was written or assisted by an AI agent. (If checked, the prompt(s) are above.)

## Checklist

- [ ] Commits are **DCO signed off** (`git commit -s`) — see [CONTRIBUTING](../CONTRIBUTING.md#developer-certificate-of-origin-dco).
- [ ] `make check` passes locally (generate + build + test).
- [ ] I followed the [layering / import rules](../AGENTS.md) — no cross-layer imports, no I/O in inner-ring packages.
- [ ] No silent fallbacks — failures surface as errors/HTTP status, not a quiet default model.
- [ ] No magic strings for provider/model names — used the `internal/providers` constants.
- [ ] Generated code (`internal/sqlc/`) was **regenerated** via `make generate`, not hand-edited.
- [ ] If adding a provider/model, I followed the [`add-provider` skill](../.claude/skills/add-provider/SKILL.md).
