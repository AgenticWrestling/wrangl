# 3c6W9Q — Archive projects: UI (design)

Interactive prototype: `prototype.html` (self-contained, fonts embedded). `#A`, `#B`, `#C` pick a variant; add a state, e.g. `#A-confirm`,
`#C-inline`, `#B-menu`, `#A-archived`, `#A-task`, `#A-list`; append `-ok` (`#A-confirm-ok`) for "no active runs" instead of the refusal.
The bottom-left toolbar switches variant/state and toggles "running run". Screenshots: `variant-{A,B,C}-<state>-{desktop,phone}.png`
(desktop 1280x800, phone 390x844 @2x); `shoot.js` regenerates them. No application code was changed.

States: `confirm`/`inline` = archive confirmation, refused (2 active runs listed, from the 409 body) or ok; `archived` = /project/<slug>;
`task` = /task/<sqid> of an archived project; `list` = where archived projects are listed with Restore; `menu` (B) = the overflow menu.

## Shared behaviour
- **Refusal is a message, not a dead end:** "devhost has 2 active runs. Archiving never orphans a running run…" with each run (id, state pill,
  task title, task link) and **View runs**. Archive isn't greyed out beforehand, so the owner always learns *why*. (No cancel-and-archive: the API refuses only.)
- **Confirm copy** says what happens: hidden from Tasks/Inbox/search, N queued runs cancelled ("project archived"), restorable from Settings.
- **Archived pages** (/project/<slug>, /task/<sqid>): an amber "Archived" notice with a primary **Restore** button; cards get dashed borders;
  create/queue/merge/edit/link-repo/ask buttons are disabled (visible, not hidden, with a one-line reason) so the page still reads as the same page.
- **Restore** is one tap, no confirm (it is harmless); a toast confirms. Cancelled queued runs stay cancelled (stated in C's banner).
- Live-page conventions: Archive/Restore post to `/ui/act/...` and the page re-renders via the usual SSE patch; the dialog is a `<dialog class="sheet">` like the existing sheets (bottom sheet on phones), Esc closes.
- Touch targets 44px on phones; danger = vermillion text/border (outline) until the final "Archive project" confirm, which is the only filled red button.

## Variants
| | Archive entry | Confirm / refusal | Archived banner | Archived list |
|---|---|---|---|---|
| A | "Danger zone" card at the bottom of the project page | modal sheet (bottom sheet on phone) | sticky full-width bar at the very top, on project *and* task pages | Settings section "Archived projects" |
| B | `•••` overflow menu next to Edit project → "Archive project…" | modal sheet; ok case asks to type the slug | strip inside the header card (travels with the card, less shouty) | collapsible "Archived projects" at the bottom of the /tasks project list |
| C | outlined danger "Archive…" button beside Edit project | inline card expands under the page (no modal, nothing to dismiss on phones) | banner card with counts + explains what Restore does | Settings section |

## Recommendation
**A** for the banner and Settings list (always visible while scrolling, nothing to learn), with **C**'s inline confirm if modals feel heavy on
phones. B's overflow menu is the tidiest header but hides a rare action well; the typed-slug confirm is overkill since archiving is reversible.
