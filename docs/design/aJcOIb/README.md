# aJcOIb — Navigation: Projects, switcher, phone pill (design + implementation)

Prototype: `prototype.html` (`#A-closed`, `#A-open`, `#B-…`, `#C-…`; resize to switch desktop/phone). Screenshots: `variant-{A,B,C}-{closed,open}-{desktop,phone}.png`;
`node shoot.js` regenerates them. (Some icon glyphs render as boxes in the screenshots: the headless browser had no symbol font.)

| | Desktop left panel | Phone |
|---|---|---|
| A (implemented) | "Projects" + five most recent projects + "All projects" link | pill at the top of the project page opens a dropdown: all projects + "All projects"; tab bar "Projects" goes to the list |
| B | same, with a project count on "All projects" | pill opens a bottom sheet with search and per-project "needs you" counts (best once there are many projects) |
| C | same, with an activity dot on projects that need you | no pill; tapping the Projects tab opens the switcher sheet (loses a direct route to the list unless "All projects" is used) |

Recommendation: A now (smallest, matches today's `details.pmore` menu); B's sheet once counts exist (Projects list task).

## Implemented
- Left panel and phone tab bar say **Projects**; the recent-projects sub-list links to `/projects/{slug}`. Phone shows the project pill (desktop keeps the project bar); the menu lists projects plus "All projects".
- Routes: `/projects` (list: projects, New project sheet, archived collapsed; counts/expandable rows come later), `/projects/{slug}` (today's Tasks page, unknown slug = 404), `/projects/{slug}/settings` (goals & repos page).
- Redirects (302): `/tasks[?project=x&all=1&now=1]` -> `/projects[/x[?…]]`, `/backlog` -> `…#backlog`, `/project/{slug}` -> `/projects/{slug}/settings`; `/` -> `/projects`. Internal links, toasts and keyboard `g` shortcuts use the new URLs. The live stream accepts both `slug` and `project`.
- e2e: existing tests use the new URLs; `web_test.go` checks the Projects list, the nav label, every redirect and the unknown-project 404.
