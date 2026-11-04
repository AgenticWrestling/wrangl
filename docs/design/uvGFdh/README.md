# uvGFdh — Desktop keyboard shortcuts: MVP (design)

Interactive prototype: `prototype.html` (open in a desktop browser; `#A`, `#B`, `#C` pick a variant; keys work: j/k, Enter, Esc, h/l, ?).
Fonts are copied into `fonts/` so it is self-contained. No application code was changed.

Shared keymap: j/k + arrows move; Enter/o toggle detail; Esc collapse then clear; h/l collapse/expand or parent/child;
q queue, e edit, c complete, n new task; ? help. Ignored with Ctrl/Cmd/Alt, in inputs/dialogs, on touch, on the terminal page.
Phone: nothing changes (keys are off on touch; no hints shown) — see `variant-*-phone.png`.

| Variant | Selection style (not colour alone) | Discovery |
|---|---|---|
| A Bar | Thick white left bar + outline + `>` margin marker, raised bg | `?` dialog (centered); `n`/`?` hints on header buttons |
| B Chips | Dashed underline + `q e c` key chips on the selected row (replace status pill) | `?` dialog; chips teach row actions |
| C Drawer | Bold title + double ring | Persistent bottom hint bar; `?` opens a right-hand cheatsheet drawer |

Recommendation: A's selection + C's hint bar is heavy; start with A and the `?` dialog, add B's chips if row actions prove hard to remember.
