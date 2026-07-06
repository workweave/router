---
name: fix-pr-reviews
description: Fetches feedback from a GitHub PR — review-thread comments, ad-hoc PR comments (including those posted as a review's body without a thread), and bot/advisory review submissions — and fixes all of them as they appear (comments before CI). Bot comment-length nits apply verbatim; comments requiring a genuine human decision are NOT auto-fixed and are escalated to the user with options grounded in existing patterns and best practices. After each fix batch, runs pre-commit validation and submits, then waits for CI only when no actionable feedback remains. Loops until all feedback is resolved AND all CI checks have completed. Use when asked to fix PR comments, address review feedback, address PR-level bot reviews (e.g. workweave-bot advisory nits), babysit a PR to merge-ready, or handle review comments.
---

# Fix PR Feedback (Loop Until Done)

Automates fixing PR feedback on a GitHub PR — review threads, ad-hoc PR comments, and bot/advisory reviews (e.g. `workweave-bot` COMMENTED-state reviews with nit lists) — then validating fixes, submitting them, **and continuously looping** until the PR is merge-ready: all feedback resolved, all reviewers done, and all CI checks complete.

**Three sources of feedback — treat all three as first-class:**

1. **Review-thread comments** (`reviewThreads` in GraphQL) — line-anchored feedback attached to a code review.
2. **Issue comments on the PR** (`gh api repos/.../pulls/.../comments`) — ad-hoc PR comments not attached to any review. These include comments posted as a review's body text (visible as the review's `body` field but with `comments: null`/zero threads) — do not skip them just because they are not in `reviewThreads`.
3. **Bot/advisory reviews** with no per-line threads but a populated review body — e.g. `workweave-bot` posting `COMMENTED` reviews that list 6-10 nit-level comment-length suggestions across multiple files. The review body's contents are themselves actionable items even though no GraphQL reviewThread exists.

Fetch all three every iteration. Do not skip the issue-comments and review-body sources just because `reviewThreads` returned items.

**Priority: feedback first, CI second.** Never sit in a CI wait while unresolved feedback exists. As soon as polling surfaces a new item, stop waiting and fix it before resuming CI.

**Do not auto-fix decisions that belong to a human.** Some items don't have one objectively correct resolution — they involve a product/architecture/scope trade-off, multiple defensible approaches, or intent only the author knows. **Never guess on these.** Classify them as **Escalate**: pause that item, surface it to the user with concrete options grounded in the existing codebase patterns and best practices, and let the human choose before you touch the code.

**Bot nit rules — still Fix category, treat as authoritative:** When a bot (e.g. `workweave-bot`, `greptile-apps[bot]`, `cubic`) suggests a concrete code-replacement via a fenced ```suggestion``` block, apply the suggested replacement verbatim. Do not paraphrase, do not push back, do not skip because "the existing wording is fine." The bot's replacement is the authoritative version of "concise." For free-form bot feedback without a `suggestion` block, triage it like any other comment: Fix if straightforward, Escalate if it requires a judgment call.

## Prerequisites

- GitHub CLI authenticated: `gh auth status`
- On the PR branch locally, or provide PR number

## Important: Commit Workflow

Follow this exact order when committing changes.

1. **Lint/format autofix** — run autofix on changed files so lint changes are included in the commit
2. **Stage specific files** — `git add <specific files>` (only files you changed + lint autofix)
3. **Commit** — `git commit -m "message"`
4. **Push** — `git push`

## Priority Order

1. **Comments first** — any unresolved, non-outdated review thread (Fix or Decline) is handled **immediately**. Do not enter a passive CI wait while actionable threads exist.
2. **Escalate genuine human decisions** — threads that need a human judgment call (Escalate category) are **never** auto-fixed. Collect them and present to the user with options; do not block the rest of the loop waiting on them unless they are the only thing left.
3. **CI second** — only when there are **zero** actionable threads, wait for the current head SHA's CI to dispatch and finish.
4. **During CI wait** — poll for new comments in parallel. If polling detects **any** new unresolved thread, **abort the CI wait** and go fix comments (back to step 1). Auto-reviewers often post while CI runs.

## High-Level Loop

```
┌──────────────────────────────────────────────────────────────┐
│ 0. Identify PR and align local branch with pr.headRefName    │
│ 1. Fetch fresh state (threads + reviews + checks for SHA)    │
│ 2. If DONE conditions met → exit                             │
│ 3. If actionable unresolved threads exist →                  │
│    a. Triage threads → Fix / Decline / Skip / Escalate       │
│    b. Fix each Fix thread → reply to declines                │
│    c. Escalate threads: ASK the user, wait for decision      │
│    d. Run pre-commit validation (NEVER skip; see Step 4)     │
│    e. Resolve threads → commit → push                        │
│    f. Go to step 1 (do NOT wait for CI yet)                  │
│ 4. Else (no actionable threads):                             │
│    a. WAIT for head SHA CI to dispatch + finish (Step 6)     │
│       — interrupt if polling finds new threads → step 1      │
│    b. Go to step 1                                           │
└──────────────────────────────────────────────────────────────┘
```

### DONE conditions (ALL must hold against the LATEST head SHA)

The loop exits only when **every** condition is true on the same poll, *after CI has actually dispatched for the current head SHA*:

1. **No unresolved review threads** — every `reviewThread` has `isResolved: true` or `isOutdated: true` (Skip-category threads count as resolved-for-loop-purposes). **Escalate threads awaiting a user decision block DONE** — do not exit while an escalated thread is still pending the human's choice. If the only remaining threads are escalations and the user has not yet responded, surface them and stop (see Step 3.5).
2. **All required CI checks for the head SHA are present and passed** — see Step 6 for how to validate dispatch. "Concluded" means the check state is `SUCCESS`, `FAILURE`, or `NEUTRAL` (not `PENDING`, `QUEUED`, or `IN_PROGRESS`).
3. **No CI checks pending or running** — `gh pr checks` shows nothing in `PENDING`, `QUEUED`, or `IN_PROGRESS` state.
4. **No reviewers with `REVIEW_REQUESTED`** — all requested reviewers have either submitted or been dismissed.
5. **No `CHANGES_REQUESTED` reviews active** — the latest review state from each reviewer is `APPROVED`, `COMMENTED`, or `DISMISSED`.

If any condition fails after a fresh fetch, run another iteration.

## Execution Flow

### Step 0: Identify the PR and align local branch

The skill is **single-PR scoped**. If the user gave a PR number, use it. Otherwise infer from the current branch.

```bash
# PR metadata (resolve PR_NUMBER, OWNER, REPO, HEAD_REF)
gh pr view "${PR_NUMBER:-}" --json number,baseRepository,headRepository,headRefName,headRefOid,isCrossRepository \
  -q '{owner: (if .isCrossRepository then .baseRepository.owner.login else .headRepository.owner.login end), repo: (if .isCrossRepository then .baseRepository.name else .headRepository.name end), number: .number, branch: .headRefName, sha: .headRefOid}'
```

Verify `git branch --show-current` matches `headRefName`. Run `git checkout <headRefName>` if not. Fixes always go on the branch of the PR whose threads you're addressing.

### Step 1: Fetch Fresh PR State (every iteration)

Re-fetch **all feedback signals** at the start of every iteration. Never reuse stale data. Three sources, all first-class:

1. `reviewThreads` — line-anchored review comments (GraphQL)
2. `review.body` + `review.comments.nodes` per review — including bot/advisory reviews with populated bodies but zero threads (e.g. `workweave-bot` listing nit suggestions in the review body)
3. Issue comments on the PR (REST `/issues/:n/comments`) — ad-hoc PR comments and any unreviewed bot commentary

```bash
# 1. Threads + reviews (with review bodies and per-review comments) + review-requests.
#    latestReviews pulls both the review.body string AND any comments posted at
#    the review level (reviews can have a populated body and zero threads).
gh api graphql -f query='
  query($owner: String!, $repo: String!, $pr: Int!, $threadCursor: String, $reviewRequestCursor: String, $reviewCursor: String) {
    repository(owner: $owner, name: $repo) {
      pullRequest(number: $pr) {
        headRefOid
        reviewThreads(first: 100, after: $threadCursor) {
          nodes {
            id isResolved isOutdated path line
            comments(first: 10) { nodes { id body author { login } createdAt } }
          }
          pageInfo { hasNextPage endCursor }
        }
        reviewRequests(first: 50, after: $reviewRequestCursor) {
          nodes { requestedReviewer { ... on User { login } ... on Team { name } } }
          pageInfo { hasNextPage endCursor }
        }
        latestReviews(first: 50, after: $reviewCursor) {
          nodes {
            id author { login }
            state submittedAt
            body      # full review-body text — bot nitlists often live here, not in threads
            comments(first: 10) { nodes { id body path line author { login } } }
          }
          pageInfo { hasNextPage endCursor }
        }
      }
    }
  }
' -f owner="$OWNER" -f repo="$REPO" -F pr="$PR_NUMBER"

# 2. Issue comments on the PR (ad-hoc, plus unreviewed bot commentary).
#    These do NOT surface in GraphQL reviewThreads.
gh api "repos/$OWNER/$REPO/issues/$PR_NUMBER/comments?per_page=100" --paginate \
  --jq '.[] | {id, user: .user.login, body, created_at}'

# 3. Per-commit review comments (REST). Authoritative source of line-anchored
#    feedback and the only path that cleanly surfaces ```suggestion``` blocks.
gh api "repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments?per_page=100" --paginate \
  --jq '.[] | {id, user: .user.login, path, line, body, created_at}'

# 4. CI checks
gh pr checks "$PR_NUMBER" --json name,state,bucket,link
```

**Note:** For production implementations, iterate through all pages:
- Continue fetching `reviewThreads` with `after: endCursor` until `pageInfo.hasNextPage` is false
- Continue fetching `reviewRequests` with `after: endCursor` until `pageInfo.hasNextPage` is false  
- Continue fetching `latestReviews` with `after: endCursor` until `pageInfo.hasNextPage` is false
- Page REST endpoints via `gh api --paginate`

For production implementations, iterate through all pages before computing DONE flags. For brevity in this skill spec, we show only the first page fetch.

**Build a unified list before triage.** A bot review whose body lists 6 nits becomes **6 actionable items**, not just one review. A PR-level ad-hoc comment is one item. A review-thread comment is one item. Merge sources 1+2+3 into a single feedback list sorted by `created_at` ascending, then triage from that list — **not** from `reviewThreads` alone.

Compute the five DONE flags. If all true, **exit the loop**.

**Important:** The GraphQL query shown fetches only the first page of results. For production use, you must paginate through all pages:
- `reviewThreads`: continue with `after: endCursor` until `!hasNextPage`
- `reviewRequests`: continue with `after: endCursor` until `!hasNextPage`
- `latestReviews`: continue with `after: endCursor` until `!hasNextPage`
- REST endpoints: `gh api ... --paginate`

Only compute DONE after collecting all pages.

**Comment-first branch:** If anything in the unified feedback list is unresolved/unactioned and is not Skip-category → go to Steps 2–5 immediately. **Do not** wait for CI first, even if checks are still running or failed.

**CI-wait branch:** Only when there are zero actionable threads → Step 6.

### Step 2: Triage Unresolved Threads

For each thread where `isResolved: false` AND `isOutdated: false`, classify into:

| Category | Type | Action |
|----------|------|--------|
| Fix | Bug fix request | Fix immediately |
| Fix | Security concern | Address with care |
| Fix | Code style/convention | Apply consistently |
| Fix | Refactoring suggestion (clear, single obvious approach) | Implement if clear |
| Fix | Nitpicks | Fix if straightforward |
| Fix | Bot comments about comment length / doc brevity | Apply as-is — the bot's suggested replacement text is authoritative; these are objective conciseness improvements, not subjective style calls |
| Decline | False positive / incorrect suggestion | Reply with explanation, don't change code |
| Decline | Already handled elsewhere | Reply pointing to where it's handled |
| Decline | Would make code worse | Reply explaining the trade-off |
| Decline | Out of scope for this PR | Reply explaining scope |
| Escalate | Genuine human decision required | **Do not change code.** Ask the user (Step 3.5) with options + a recommendation |
| Skip | Pure questions / discussion | Don't change code, don't resolve |

**Fix vs Escalate — the key distinction.** A comment is a **Fix** only when there is a single, objectively-correct resolution that matches existing codebase patterns. A comment is an **Escalate** when resolving it requires a judgment call a human must own. Signals that a thread is **Escalate**, not Fix:

- **Product / UX / behavior change** — the "right" behavior is a product decision, not a code one.
- **Architectural trade-off** — multiple defensible approaches with real, lasting consequences (e.g. new abstraction vs. inline, sync vs. async, schema/migration shape).
- **Scope expansion** — fixing it properly means doing meaningfully more than the PR set out to do.
- **Ambiguous intent** — only the author knows why the code is the way it is, and the comment may be based on a wrong assumption.
- **Conflicting reviewers** — two reviewers want opposite things on the same thread.
- **Risk / blast radius** — the change touches security boundaries, data integrity, billing, or migrations in a way where guessing is unacceptable.

When in genuine doubt between Fix and Escalate, **Escalate** — it is cheaper to ask than to ship the wrong opinionated change.

### Step 3: Address Each Comment

#### For "Fix" comments

1. **Read the file** at the specified path
2. **Locate the line** referenced
3. **Understand the context** from the diff
4. **Apply the fix** — minimal change requested
5. **Verify the change** makes sense

Guidelines:

- Make only the changes requested
- Don't refactor unrelated code
- If unclear, **escalate to the user** rather than guessing. The Escalate category exists for cases where intent is ambiguous or product decisions are needed.

#### For "Decline" comments

Reply to the thread with a concise explanation, then resolve.

```bash
gh api graphql -f query='
  mutation($body: String!, $pullRequestReviewThreadId: ID!) {
    addPullRequestReviewThreadReply(input: {body: $body, pullRequestReviewThreadId: $pullRequestReviewThreadId}) {
      comment { id }
    }
  }
' -f body='[Your explanation]' -f pullRequestReviewThreadId='{THREAD_ID}'
```

Reply guidelines:

- Be respectful and specific — explain *why*, don't just say "won't fix"
- Reference the relevant code or context that supports your reasoning
- Keep it to 1-3 sentences

#### For "Skip" comments

Leave these threads alone — no code change, no reply, no resolve. These are discussions for the PR author to handle.

#### For "Escalate" comments

**Never change code or resolve the thread on your own.** Instead, research first, then ask the human.

1. **Investigate before asking** — read the referenced file/line, the surrounding code, and search the codebase for how similar cases are already handled. The user should not have to do this legwork.
2. **Frame the decision** for each escalated thread:
   - What the reviewer is asking for (1 sentence) and a link to the thread.
   - Why it needs a human (which Escalate signal above it hits).
   - 2–4 concrete options, each grounded in **existing patterns / best practices** found in step 1, with the trade-offs of each.
   - Your **recommendation** and why — be opinionated, but make clear it's the human's call.
3. **Present all escalations in one batch** using `AskQuestion` (one question per thread) so the user can decide quickly. Do not drip them out one at a time across iterations.
4. **Wait for the user's decision.** Once they choose, apply it exactly (treat the chosen option as a Fix), reply on the thread noting the decision, then resolve. If the user defers ("skip for now"), leave the thread unresolved and note that the PR is blocked on it.

Do not silently auto-resolve an escalated thread, and do not let it slip through as a Fix because it felt easier than asking.

### Step 3.5: Batch and Surface Escalations

If this iteration produced any **Escalate** threads, after handling all Fix/Decline threads:

1. **Announce** to the user how many escalated threads exist and that they block merge-ready status.
2. Use `AskQuestion` with one entry per escalated thread, each carrying the framing from Step 3 (reviewer ask, why it needs a human, options grounded in existing patterns, your recommendation).
3. If the user has **not yet responded** and the only remaining work is escalations, **stop the loop** and hand back to the user — do not spin iterations or sit in a CI wait waiting on a human. Resume from Step 1 once they answer.
4. Apply each decision as a Fix, reply on the thread referencing the decision, resolve, then continue the normal Step 4 → Step 5 flow.

### Step 4: Pre-Commit Validation (MANDATORY — never skip)

**This step exists because lint, format, and test errors that take 1 second to fix locally take ~5 minutes to fail in CI and trigger another loop iteration.** Skipping this step is the single biggest cause of unnecessary iterations.

Run these checks **on every iteration that touched code**, before commit:

1. **Lint/format autofix** — run autofix on changed files
2. **Type check** — run type checking on changed files
3. **Tests** — run tests if logic changed
4. **Generated code drift** — if codegen files changed, regenerate and commit

**For full details, consult your project's pre-commit validation guide.** This skill section is the minimum that MUST run inline. Do not delegate-then-skip.

### Step 5: Resolve Threads, Commit, Push

Resolve all "Fix" and "Decline" threads, plus any "Escalate" threads the user has already decided on (not "Skip", and not Escalate threads still awaiting a decision):

```bash
gh api graphql -f query='
  mutation($threadId: ID!) {
    resolveReviewThread(input: {threadId: $threadId}) { thread { isResolved } }
  }
' -f threadId='{THREAD_ID}'
```

For bot reviews with nitlist bodies: post a single reply on the review summarizing "applied N/M suggestions with these commits" so the bot/author can audit coverage without re-running the whole list.

For ad-hoc issue comments that are not in any thread (rare, but they happen for first-pass bot reviews that pre-date threaded mode): there's no thread to resolve — the comment just sits on the PR. Track them in your unified list and mark them as "addressed in commit X" when you push.

For threaded review comments: resolve per-thread via the GraphQL mutation below. Then commit and push. Skip if no files changed (e.g. all-Decline iteration):

```bash
# 1. Stage specific files only
git add path/to/fixed1.go path/to/fixed2.tsx

# 2. Commit
git commit -m "fix: address PR feedback (iteration N)

Fixed:
- [summary of fix 1]
- [summary of fix 2]

Applied from <bot-name> review:
- [summary of bot-fix 1]
- [summary of bot-fix 2]

Declined (with explanation):
- [summary of declined 1]"

# 3. Push
git push
```

**Capture the new head SHA** after push — you need it for Step 6:

```bash
NEW_SHA=$(git rev-parse HEAD)
```

### Mandatory post-push feedback audit (do not skip)

Immediately after `git push` succeeds, **go back to Step 1** before Step 6. Do not enter CI wait on the same iteration you pushed.

Auto-reviewers (`cursor[bot]`, `greptile-apps[bot]`, `cubic`, `workweave-bot`, etc.) often file **new** feedback on the new commit asynchronously — both review-thread comments and review-body nitlists. Treat "zero actionable feedback right before push" as meaningless for the new SHA.

Count actionable items across **all three sources** (reviewThreads + review.body nitlist splits + REST issue & PR comments):

```bash
# 1. Unresolved review-thread comments
gh api graphql ... # same query as Step 1 | \
  jq '[.data.repository.pullRequest.reviewThreads.nodes[] \
        | select(.isResolved == false and .isOutdated == false)] | length'

# 2. Unresolved threads in review bodies (review comments, not thread comments)
gh api graphql ... # same query as Step 1 | \
  jq '[.data.repository.pullRequest.latestReviews.nodes[] \
        | .comments.nodes[] \
        | select(...) ] | length'

# 3. Unactioned issue comments (POST-push only — pre-push already covered these)
gh api "repos/$OWNER/$REPO/issues/$PR_NUMBER/comments?per_page=100" --paginate | \
  jq '[.[] | select(. created_at > LAST_PUSH_SHA_TIMESTAMP)] | length'
```

A unified actionable count > 0 → triage/fix (Steps 2–5). Only when count == 0 across all three sources → Step 6 CI wait.

**Note:** Skip-category threads should be excluded from "actionable" for CI-wait decisions. In practice:
- If you explicitly classified an item as Skip earlier in this iteration, it should not count as actionable
- For the post-push audit, assume any unresolved non-outdated item needs attention unless you've already classified it as Skip

If `git push` errors with "branch is out of date with remote" or similar after a previous PR in the stack merged, run `git fetch origin` → `git rebase origin/main` (or appropriate base) if needed, then retry `git push`.

### Step 6: Wait for CI (only when no actionable threads)

**Enter Step 6 only when Step 1 found zero actionable unresolved threads.** Comments always preempt CI — this is absolute: the **instant** a poll surfaces a new actionable thread, **abort the CI wait immediately** (do not finish the current poll's CI check, do not wait for the run to conclude) and return to Step 1. Treat "a comment exists" as a hard interrupt, not a queued item.

A naive `gh pr checks` immediately after push will report `pending=0, failed=0` because GitHub has not dispatched checks yet for the new SHA. Anchor on the head SHA and validate dispatch before treating CI as done.

#### Interruptible CI wait (required)

Do **not** use a single blocking `gh pr checks --watch` without also polling for new review threads. Use a manual loop that on **every** poll interval:

1. **Re-fetch review threads** (same GraphQL query as Step 1).
2. If **any** new unresolved, non-outdated, non-Skip thread exists → **break immediately**, announce thread count to the user, go to **Step 1** (fix comments). Do not wait for CI to finish first.
3. Else check CI state for `HEAD_SHA` (current `headRefOid` from Step 1).

```bash
HEAD_SHA=$(gh pr view "$PR_NUMBER" --json headRefOid -q .headRefOid)

# For cross-fork PRs, use headRepository; otherwise use baseRepository
# (check-runs API needs the repo where the head commit lives)
CHECKS_OWNER="$OWNER"
CHECKS_REPO="$REPO"

# Step 6.1: Wait for CI to DISPATCH for HEAD_SHA (~30-120s after push).
DISPATCH_TIMEOUT=false
for i in $(seq 1 12); do
  sleep 15
  # --- comment interrupt (run GraphQL; if actionable threads > 0 → exit 1) ---
  count=$(gh api "repos/$CHECKS_OWNER/$CHECKS_REPO/commits/$HEAD_SHA/check-runs" --jq '.total_count')
  echo "[dispatch poll $i] checks for $HEAD_SHA: $count"
  # Dispatch detected when at least one check exists (some repos have fewer required checks)
  if [ "$count" -gt 0 ]; then break; fi
  if [ "$i" -eq 12 ]; then DISPATCH_TIMEOUT=true; fi
done

# If no checks exist (e.g., repo has no required checks), treat as dispatched immediately
# Note: count will be 0 if checks never existed OR if they were all cleared
if [ "$count" -eq 0 ] && [ "$DISPATCH_TIMEOUT" != true ]; then
  echo "[info] No checks configured or none dispatched; treating as dispatched"
fi

if [ "$DISPATCH_TIMEOUT" = true ]; then
  echo "[warning] CI dispatch timeout: no checks found for $HEAD_SHA after 3 minutes"
fi

# Step 6.2: Poll completion + comments. Cap ~30 min.
for i in $(seq 1 60); do
  sleep 30
  # --- comment interrupt FIRST each iteration (GraphQL; actionable threads → exit 1) ---
  state=$(gh pr checks "$PR_NUMBER" --json state -q '[.[] | .state] | unique | join(",")')
  echo "[completion poll $i] states: $state"
  if ! echo "$state" | grep -qE 'PENDING|QUEUED|IN_PROGRESS'; then break; fi
done
```

Implement the comment interrupt in the agent loop: after each sleep, re-run the Step 1 GraphQL fetch; if actionable threads exist, **stop CI polling** and triage/fix before the next CI poll.

Use shorter poll cycles (30s) so new comments are picked up quickly.

#### When CI polling completes (and no new threads interrupted)

1. **Read failing check logs** for any `bucket: fail` rows:

   ```bash
   gh run view <run-id> --log-failed 2>&1 | grep -E 'error|Error|##\[error\]|FAIL' | head -40
   ```

   Failing checks become "issues to fix" — treat them as Fix-category items in the next iteration's Step 2 triage, then address them via Steps 3-5.

2. **Re-fetch review threads** one more time before re-evaluating DONE (auto-reviewers may have posted at the end of the CI window).

3. Go back to **Step 1**.

### Step 7: Loop Termination

Stop when Step 1's DONE check passes against the LATEST head SHA. Print:

```
PR #<num> ready to merge:
  - Resolved threads this run: <count>
  - Declined threads this run: <count>
  - Escalated → user-decided this run: <count>
  - CI checks: all green (<count> checks)
  - Reviewers: <list> (all approved or commented)
```

If after **5 full iterations** the loop hasn't terminated, stop and surface the blocker to the user (likely a reviewer keeps re-requesting changes on the same point, or a flaky test). Don't loop forever.

## Anti-Patterns (lessons learned)

| Anti-Pattern | Why it bit us | What to do instead |
|--------------|---------------|--------------------|
| Waiting for CI before fixing review comments | Reviewers blocked on feedback while agent watches checks | **Comments first:** fix all actionable threads, then Step 6 CI wait |
| Sitting in CI wait without polling for new threads | Auto-reviewers post during CI; comments sit for minutes | Interrupt CI wait every poll; new thread → Step 1 immediately |
| Polling CI immediately after `git push` | Returns `pending=0` before dispatch → false DONE | Anchor on `commits/$HEAD_SHA/check-runs` count; validate dispatch |
| Treating "0 threads at push time" as "0 threads forever" | Auto-reviewers post async, sometimes minutes after push | Poll threads during CI wait; re-fetch after CI completes |
| Pushing without running all validations | Errors only surface in CI, triggering another loop | Always run validation checks before push |
| Conflating PRs in a stack | Comments on PR #2 fixed on PR #1's branch | Verify `git branch --show-current == pr.headRefName` each iteration |
| Delegating pre-commit-validation to "another skill" and forgetting to run it | Trust gap — easy to skip when changes feel small | Inline the minimum checks in this skill (Step 4); fully run them, no exceptions |
| Auto-fixing a comment that's really a product/architecture/scope decision | Shipped an opinionated change the author didn't want; wasted review cycles | Classify as **Escalate**; ask the user with options + recommendation (Step 3.5) |
| Escalating but guessing anyway while waiting | Defeats the point of escalating | Leave the thread unresolved; do not touch the code until the human decides |
| Asking the user with no research or options | Forces the human to do the legwork; slow decisions | Investigate existing patterns first, then present 2–4 grounded options + a recommendation |
| Fixed batch 1, pushed, then CI-wait without re-fetch | Bugbot posts new threads on the fix commit; user sees open comments | **Post-push thread audit** every time; Step 1 before Step 6 on same iteration |
| Resolved one thread and stopped babysitting | Other threads still open; lazy-pr-fix incomplete | Resolve/reply all actionable threads; print termination summary only when DONE |
| Ending session during CI poll | Miss late bot comments and failing checks | Resume with Step 1 thread fetch; continue interruptible CI loop |
| Treating bot `COMMENTED` reviews as "no feedback" because `reviewThreads` is empty | Missed the entire nitlist (e.g. workweave-bot's 6-file comment-length fixup on PR #580) | Always fetch `review.body` + REST issue comments; bot nitlists live in bodies, not threads |
| Rephrasing the bot's suggested replacement instead of applying it verbatim | Lost the "concise" wins the bot was pushing for; subjective drift on what "concise" means | Apply the bot's fenced ```suggestion``` block as-is — never paraphrase, never reword |
| Closing the loop after the last review-thread comment resolves | Bot advisory review body still has open nits filed against it | Treat each nit in a bot review body as its own actionable item; count individually |

## Error Handling

| Issue | Solution |
|-------|----------|
| Comment references deleted line | Check git history, apply fix to current location |
| File was renamed | Find new path, apply fix there |
| Conflicting comments | Address most recent, note conflict in reply |
| Fix breaks tests | Revert, try alternative approach |
| CI check stuck `IN_PROGRESS` >30min | Stop polling that iteration, surface to user |
| Reviewer keeps re-requesting changes on same point | After 2 declines on the same thread, surface to user instead of looping |
| `git push` rejected (rebase conflict / merged dependency) | `git fetch` → `git rebase origin/main` → `git push` |
| Git submodule issues | Run `git submodule update --init --recursive` |

## Notes

- **Skip-thread persistence:** Skip-category threads are intentionally left unresolved (they're discussions, not action items). Without storing which threads were classified as Skip, the same unresolved discussion may be re-triaged on subsequent iterations. For production implementations, store skipped thread IDs in memory for the duration of this skill run.
