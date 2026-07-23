---
name: triage-issue
description: First-pass triage procedure for a new GitHub issue on workweave/router — dedupe search, docs check, label taxonomy, and a transparent first response. Used by the claude-issue-triage workflow and by maintainers triaging by hand. Use when triaging or labelling a router issue.
---

# Triaging a router issue

Goal: give the reporter an instant, useful, honest first response and leave the
issue correctly labelled — **without pretending to be a human and without
claiming anything is fixed.** A maintainer follows up; you set them up.

## 1. Read it

Read the title and body. Identify the type (bug / feature / provider request /
docs / question) and whether the essential info is present (for a bug: version,
repro, expected-vs-actual).

## 2. Dedupe

Search existing issues before responding:

```bash
gh issue list --repo workweave/router --state all --limit 60 --search "<keywords>"
```

If there's a strong match, link it in your comment and apply the `duplicate`
label. Don't close it yourself — leave that call to a maintainer.

## 3. Check against the docs

If it's answered by [`README.md`](../../../README.md) or
[`docs/CONFIGURATION.md`](../../../docs/CONFIGURATION.md), point to the exact
section. A large share of issues are configuration questions — resolving them
with a precise pointer is the highest-value thing you can do.

## 4. Label

Apply labels with `gh issue edit`. Use **only** this set — do not invent labels:

| Label | When |
|---|---|
| `bug` | Something is broken / misbehaving. |
| `enhancement` | New capability or improvement. |
| `provider-request` | Add a provider/model. Point them at the [`add-provider` skill](../add-provider/SKILL.md). |
| `documentation` | Docs are wrong/missing. |
| `question` | Usage question, not a defect. |
| `needs-info` | Can't act without more detail (missing repro/version/logs). |
| `duplicate` | Strong match to an existing issue (linked). |

(`provider-request` and `needs-info` are router-specific; the rest are GitHub
defaults. If a label doesn't exist yet, skip it rather than failing.)

## 5. Respond — exactly one comment

Post one comment that **opens with this transparency line verbatim**:

> 🤖 _Automated first-pass triage — a maintainer will follow up. Reply with `@claude` if you need the agent again, or tag a maintainer if you need a human sooner._

Then, briefly:
- One line restating the problem as you understand it.
- Any duplicate or docs links you found.
- Only if essential info is missing: a short, specific list of what you need
  (e.g. "router version + the exact request that failed").

Tone: friendly coworker, not support bot. Hard rules:
- **Never** say it's fixed or promise a timeline.
- **Never** dump logs, internal implementation details, or speculation.
- For a feature request, capture the **underlying goal**, not just the proposed
  code change — the maintainer may solve it differently.
- If you're genuinely unsure what it is, label `question` and say a maintainer
  will take a look. Honest beats confident-and-wrong.
