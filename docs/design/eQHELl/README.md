# eQHELl — Projects list (design + implementation)

Prototype: `prototype.html` (`#A`, `#A-form`, `#B`, `#C`; resize for phone). Screenshots: `variant-{A,A-form,B,C}-{desktop,phone}.png`; `node shoot.js` regenerates them. (Icon glyphs show as boxes: the headless browser had no symbol font.)

| | Desktop | Phone | Trade-off |
|---|---|---|---|
| A (implemented) | table (Runners, In progress, Queued, Backlog, Inbox); click a row to expand about / runners now (with "also serving X") / recent activity / Open, Comment, Edit, Archive | the same rows become expandable cards | same pattern as the Logs table; counts scan well; details on demand |
| B | card grid with count chips and a stacked load bar | cards stack | friendlier for few projects, poor for scanning/sorting many, no room for actions |
| C | dense table, no expansion; Open button and a ⋯ menu | cramped (7 columns) | fastest to scan, but shows no runners-now or activity |

Recommendation: A.

## Implemented
- `/projects`: `web/projects.go` aggregates counts with a few queries (not one per project): runners = distinct runners hosting an active run; in progress = active runs; queued = queued runs; backlog = open top-level tasks with no active/queued run; Inbox = open questions + open PRs. Also runners-now per project with "also serving" tags for runners that host other projects too, and the three latest touched tasks.
- Expandable rows use the Logs pattern (a signal per row, so an open row survives live patches). On phones the same markup is restyled into cards (CSS only).
- "＋ New project" toggles an inline form (same `project-new` action). Archive uses the inline confirm and a new `project-archive` action (refusals for active runs show as a toast); archived projects sit in a collapsed section with Restore (`project-restore`). Comment links to the project page until the chat task lands.
- e2e (`web_test.go`): table structure, expand content, confirm attribute, inline form, archive/restore round trip. Not covered: exact count values.
