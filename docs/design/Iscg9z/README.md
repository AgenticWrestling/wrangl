# Iscg9z — Backlog filter and collapsed parents

Prototype: `prototype.html` (self-contained; open it, switch variants in the top bar, try typing, the type control, Show done, expand chevrons, and "simulate live update"). Images: `variant-{A,B,C}-{collapsed,filtered,types,done,empty}-{desktop,phone}.png`. `node shoot.js` regenerates them.

## Shared behaviour (all variants)
- **Parents collapsed by default**, with a pill showing the child count ("4 subtasks"). Clicking the chevron or the pill expands. When "Show done" is on the pill also shows a mini progress bar and `done/total`; when off, the count excludes done children.
- **Filter semantics**: text matches title or sqid prefix (matched text highlighted); type filter is multi-select. A parent whose descendant matches stays visible as dimmed context and auto-expands to reveal the matches; a manual collapse is respected. A summary line says "N of M tasks match" with Clear filter. If nothing matches, the empty state offers Clear filter and, when relevant, "Include done".
- **Show done toggle replaces the separate "show done" link**; done tasks are struck through and dimmed.
- **Persistence**: filter state lives in the URL (`?q=bug&type=ux,bug&done=1`, shown in the prototype as the hash), so reload, back/forward and sharing keep it. Live updates re-render rows in place and never touch the filter; a quiet "N new since you filtered" note appears instead of resetting. Expand/collapse state is per-session and is not cleared by live updates (only by changing the filter text).
- Drag-reorder handles stay; while a filter is active reordering should be disabled (order among a subset is ambiguous) — handles dim.

## Variants
- **A — Toolbar.** One row: search field, a "Type: all ▾" dropdown (multi-select checklist with per-type counts), and the Show done switch. Compact, scales to many types, the dropdown hides the type list until needed. Phone: switch wraps to its own line.
- **B — Type chips.** Search plus switch on one row, and an always-visible row of type chips with counts (toggle each). Fastest for "show me only bugs" — one tap, and counts show what exists — at the cost of ~40px of permanent height (two lines on phone).
- **C — Single field.** Minimal: search field, a type button with a count badge, and a "Done" toggle button, all one line even on phone. Selected types appear as removable chips beneath only when active. Best for phones; the type and done controls are less self-explanatory.

Recommendation: A for desktop clarity; B if filtering by type is the main use; C if phone is the priority.
