# EQu2ry — Ask discussion: close control (design)

Interactive prototype: `prototype.html` (open in a browser; `#A`, `#B`, `#C` pick a variant, `#A-closed` etc. start closed).
Fonts are in `fonts/`, so it is self-contained. No application code was changed.
Screenshots: `variant-{A,B,C}-{desktop,phone}[-closed].png` (desktop 1280x800, phone 390x844 @2x).

## Shared behaviour (all variants)
- The panel is the task page's **Conversation** card (the thread with the controller agent).
- **Closing only hides it.** The thread is untouched: `wrangl task thread` still shows it and reopening shows the full history.
- **Esc and the reply box (no conflict with keys.js):** keys.js already ignores keys while focus is in an input. Esc in the reply box
  just leaves the box (the draft is kept, a toast says "Esc again to close"); Esc with focus elsewhere closes the panel. So Esc never
  throws away typing, and the backlog's Esc chain (close detail → collapse → clear filter → deselect) only runs when the Ask panel is closed.
- **Reopen:** the `Ask` button (and the `a` key when no input has focus) toggles it; closing returns focus to the Ask button, reopening focuses the reply box.
- Close button is a real button with an accessible name ("Close conversation (Esc)"), 36px target, visible focus ring; the `Esc` chip is shown on desktop only (no keyboard on a phone).

## Variants
| | Open | Closed | Phone | Trade-off |
|---|---|---|---|---|
| A Header ✕ | Close button (✕ + `Esc` chip) in the card header | Slim bar: "Conversation · 4 messages · Reopen" in the same place, so the page doesn't jump | Same card, ✕ without chip; bar is full width | Simplest, closest to today's card; keeps a bar of clutter when closed |
| B Drawer | Right-hand drawer (380px) over the page, ✕ + `Esc` in its header | Nothing in the page; a floating **Ask · 4** pill (amber dot = unread agent reply) | Bottom sheet (78% height) with a grab handle; tapping the dimmed page or ✕ closes | Page gets full width while closed and the thread no longer pushes Subtasks down; one more layer to learn |
| C Collapse | Chevron (collapse) at the card's left; clicking the header also toggles | Card stays, shows a one-line preview of the last message (truncated) | Same; whole header row is a 44px-high tap target | Never loses context, no new element; but it still takes room in a long page |

## Recommendation
Ship **A** first (smallest change, same mental model as today); move to **B** if the thread keeps pushing the Subtasks list off-screen on
the phone. C is a good fallback if you want the last reply visible at a glance.
