# EpF2ry — Inbox: filter by project with counts (design)

Interactive prototype: `prototype.html` (self-contained). `#A`, `#B`, `#C` pick a variant; optional screen and filter, e.g. `#A-inbox-pnp`,
`#B-inbox-all-open`, `#C-projects`, `#A-project`. Bottom-left toolbar switches variant/screen, adds a live "wrangl question", resets.
Screenshots `variant-{A,B,C}-{inbox-all,inbox-pnp,inbox-wrangl,projects-all,project-all}-{desktop,phone}.png` (+ `variant-B-inbox-all-open-*`);
`shoot.js` regenerates them. No application code changed.

Sample data: pnp 2, wrangl 0, devhost 1 (zero state is shown on purpose).

## Shared behaviour
- Counts update live (SSE-driven in the app): answering a card or a new question arriving bumps the count with a short pulse; the filtered list updates in place.
- Zero-count projects stay selectable (dimmed/dashed) so the badge link from Projects never dead-ends; the empty state says
  "Nothing needs you in wrangl · Show all (3)".
- Projects table Inbox badge → Inbox filtered to that project (even at 0). Project page "Needs you" strip gets **Open Inbox →** doing the same.
- Last filter is remembered per session (cookie/session storage); it is restored when opening Inbox from the nav. A link-driven filter overrides and replaces it.
  A quiet "Filter remembered for this session · Show all" line makes the persistence visible and reversible.
- In "All" view each card shows a clickable project pill; click it to filter.

## Variants
- **A — Chips row.** One tap, all counts visible at once, scrolls horizontally on phone. Best at-a-glance; costs a row of height; many projects overflow.
- **B — Dropdown.** Compact "Project: All projects 3 ▾" button; menu on desktop, bottom sheet on phone. Scales to many projects; counts hidden until opened.
- **C — Side rail.** Desktop: left rail with counts and share-of-inbox bars; phone falls back to B's sheet. Richest on desktop; takes horizontal space.

Recommendation: A for few projects (typical), with B's sheet as the overflow when chips exceed the row.
