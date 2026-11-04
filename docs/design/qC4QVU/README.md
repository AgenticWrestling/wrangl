# qC4QVU — Surfacing PR conflicts in Inbox, task page and CLI (design)

Interactive prototype: `prototype.html` (`#A`, `#B`, `#C` pick a variant; buttons in the top bar switch the PR state: clean / conflicts / blocked / checking). Fonts are in `fonts/`. No application code was changed.

Shared across variants
- Tag **conflicts** in a new colour (bright orange `#ff8a4c`, lighter than error vermillion and distinct from attention amber), always with a branch glyph and the word. Not colour alone.
- Other states: `✓ mergeable` (green), `blocked` (yellow, dashed border), `checking` (grey, dotted; GitHub still computing).
- On a conflicted PR "Approve & merge" is disabled and **Smart Merge** (CLI: `wrangl pr resolve <id>`) is the primary action. Conflicted files are listed when known.
- CLI: `wrangl pr list` gains a CONFLICT column (`CONFLICTS` in orange on a TTY, `blocked`, `…`, `—` for closed); `wrangl pr show` gets a `mergeable` line and a `next:` hint.
- Task page: PR timeline records "Conflicts with main detected" (and later the Smart Merge outcome).

| Variant | Idea | Trade-off |
|---|---|---|
| A Tag + banner | Tag by the title plus an orange banner that explains the fix, lists files, notes whether auto-resolve is on | Clearest explanation; takes the most space |
| B Readiness checklist | Checks / review / conflicts as ticked rows; the failing row is the conflict | Shows all merge blockers uniformly (also covers `blocked`); clean PRs get a longer card |
| C Quiet tag, loud action | Orange left edge and one full-width "Resolve conflicts with Smart Merge" button on phone; Approve folds away | Fastest path to action, least explanation |

Recommendation: A's banner on the Inbox card (where the owner decides), B's checklist rows on the task page, C's full-width button on phone.

Files: `variant-{A,B,C}-{desktop,phone}.png` (conflicts) and `...-clean.png` (clean PR for comparison).
