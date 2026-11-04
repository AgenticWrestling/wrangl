# 1x0fji — Project page shell (design + implementation)

Prototype: `prototype.html` (`#A`, `#A-open`, `#B`, `#C`, `#C-menu`; resize for phone). Screenshots: `variant-{A,A-open,B,C,C-menu}-{desktop,phone}.png`; `node shoot.js` regenerates them. Mock data has 8 runners, 4 of them shared, to show the list copes with many.

| | Strip | Runners | Trade-off |
|---|---|---|---|
| A (implemented) | title, description, then chips: repos, "runners N active", concurrency, auto-merge, Edit goals & repos, Archive (right) | popover list (scrolls; bottom sheet on phone) | everything visible at once; the strip wraps to 3 rows on a phone |
| B | same chips | the runners chip toggles a panel under the strip (two columns) | easier to read many runners and keeps the page context, but pushes sections down |
| C | one quiet line (repos, counts) + ⋯ menu holding Edit, Runners, Archive | via menu | tidiest, but hides Archive and the runners count's meaning |

Recommendation: A, moving to B's inline panel if the runner list regularly passes ~10.

## Implemented
- `/projects/{slug}`: header (name + slug, first line of the description), strip (linked repos, `runners N active` popover, concurrency = per-template caps (project cap bounded by the template ceiling), auto-merge, Edit goals & repos, Archive with inline confirm), In progress / Queue / Backlog as before (run rows already carry the parent-jump tag), "show done" under the Backlog. The old "Project goals & repos" chip is gone.
- Runners never assumed single: each runner with an active session for the project is listed with its sessions; runners that also host another project's sessions are tagged "also serving X".
- API: `GET /api/v1/projects/{slug}/runners` (`proto.ProjectRunner`), `client.ProjectRunners`, `projects.Service.Runners`.
- e2e: `TestTwoSessionsShareOneRunner` checks the strip, "runners 1 active", the show-done placement and the API (one runner, two sessions); shared-runner tagging is not covered by an e2e test.
