# lz37U7 — Sidebar recent projects: recent-activity order, live dots

Open `prototype.html#A`, `#B` or `#C` (add `-held` for the frozen state, `-sheet` for C's phone sheet). "Simulate activity" fakes an SSE event; hover the list first to see the freeze.

Shared rules: max 5 entries; sorted by latest activity (run, PR, task change); blue pulsing dot = run in progress (hollow = idle; dot has a title/count so colour isn't the only signal; pulse stops under reduced-motion); live updates never reorder while the pointer is over the list — a pill offers the new order, applied on pointer-leave or click.

- **A — Quiet list**: name, dot, age. Smallest footprint. Pill "↑ order changed".
- **B — Why it moved**: two-line rows with the activity reason and a live-run count. Most informative, tallest.
- **C — Phone-first**: compact list on desktop; on phones a chip strip above the page + "Recent" bottom sheet (no hover on touch, so re-sort live while closed, freeze while open).

PNGs: `variant-<V>[-held|-sheet]-{desktop,phone}.png`. Regenerate with `shoot.js`.
