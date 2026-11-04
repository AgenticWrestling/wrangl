# 3c7W9Q — Projects table: health status and activity sparkline (design)

Interactive prototype: `prototype.html` (open directly; `fonts/` sits beside it; `#A`, `#B`, `#C` pick a variant, or use the bottom-left switcher).
Screenshots: `variant-{A,B,C}-{desktop,phone}.png` (1280px; 390px @2x). `shoot.js` regenerates them. No application code was changed.
The claude.ai artifact in the brief could not be opened from this environment, so these follow the brief's text: one status dot per project plus a merged-PRs-per-week sparkline, nothing else new.

## Status rules (one function: `projectStatus` in the prototype; port it as a single Go function and table-test it)
Precedence, first match wins:
1. **Blocked on you** (▲ orange): Inbox > 0, i.e. an open question or open PR needing the owner (this is the existing Inbox count).
2. **Running** (● blue): at least one active run (In progress > 0).
3. **Idle, work queued** (○ ring, yellow): nothing running, Queued > 0.
4. **Quiet** (— grey): none of the above. Backlog alone is still quiet: nothing has been queued.
Each state differs by shape as well as colour, and carries a text label (visible, tooltip, and `aria-label` "Status: Running. 2 runs active"). A legend sits above the table.
The prototype self-checks the rules with `console.assert` (blocked beats running; queued only when idle).

## Sparkline
12 weekly bars of merged PRs, oldest to newest; the current (partial) week is green, empty weeks are a hairline so "0 merged" reads differently from "no data". Bar height is scaled per project, so it shows shape; the total (12 weeks) is printed beside it where there is room. `aria-label`: "44 PRs merged in the last 12 weeks, 7 this week".

## Variants
| | Status | Sparkline | New columns | Phone |
|---|---|---|---|---|
| A | dot + word in a leading **Status** column | trailing **Merged / week** column with total | 2 | status sits under the name; sparkline top-right; counts become one line |
| B | dot only, leading the name (word on hover/focus and for screen readers; legend explains it) | narrow column right after the name | 1 | dot beside name, sparkline top-right |
| C | dot + word on a sub-line under the name | same sub-line, with total | 0 | identical to desktop sub-line |

## Recommendation
**C** respects "do not add columns" literally and keeps the name column the anchor; the sub-line costs ~20px of row height. **A** is the easiest to scan and sort later (status column is a natural sort key). **B** is the densest but leans on the legend, which is the weakest accessibility trade.
