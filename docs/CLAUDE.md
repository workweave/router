# docs — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

All design + plan docs for the router subproject. Read [root CLAUDE.md](../CLAUDE.md) first.

## Index is load-bearing

Every Markdown doc under `router/docs/` (active or archived) is indexed in [`README.md`](README.md). When adding a new doc, the same change must update the index — drift between the doc tree and the index = bug.

## Adding a doc

1. **Top of new file:** include the standard two-line header before the H1:

   ```
   Created: YYYY-MM-DD
   Last edited: YYYY-MM-DD
   ```

   `Created` date is load-bearing — `README.md` orders the TOC by it. Don't backdate; if the doc takes multiple days, leave `Created` on the day it first landed and bump `Last edited` as it changes.

2. **Append a row to [`README.md`](README.md)** in the correct section (Active or Archived), keeping each table sorted by `Created` ascending. Write a one- or two-sentence summary covering what the doc is for and (for archived) why archived plus link to its active replacement.

3. **If archiving an active doc:** move the row from the active table to archived with a short reason, mirror the entry in [`plans/archive/README.md`](plans/archive/README.md). Move the file with `git mv` so history follows.

4. **Renaming or deleting a doc:** update both this rule's index and inbound links. `grep -rn 'old/path' router/` before merging.
