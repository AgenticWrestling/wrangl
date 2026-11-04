# OIcACY — Inline confirm (design + implementation)

Prototype: `prototype.html` (self-contained, font in `fonts/`). `#A-ask`, `#B-ask`, `#C-ask` show the confirming state, `-idle` the resting state.
Screenshots: `variant-{A,B,C}-{idle,ask}-{desktop,phone}.png` (desktop 1000x560, phone 390x700 @2x); `node shoot.js` regenerates them (needs playwright).
Each shows a task-page Delete and an Inbox-card PR action row.

| | Interaction | Phone | Cost |
|---|---|---|---|
| A (implemented) | Button is replaced in place by "Question? [Yes] [Cancel]"; Cancel has focus, Esc / click-away reverts | question wraps above 40px buttons, nothing shifts below | one extra tap, row grows only slightly |
| B | Button "arms": label becomes "Delete?" with a ✕, reverts after 5 s | compact, but the destructive label sits where the thumb just tapped (double-tap risk) | no question text, so no room for consequences |
| C | Button opens a panel under the row with a consequence sentence | clear, but pushes content down | heaviest; good for rare, very destructive actions |

**Recommendation: A**, with C's consequence text available later through an optional detail line for archive / delete of big trees.

## Implementation
- `data-confirm="Question?"` (+ optional `data-confirm-yes="Label"`) on any button, handled by a capture-phase listener in `static/app.js`; Yes re-fires the original click so Datastar `@post`, form submit and links behave as before. `window.wrConfirm(el, prompt, yesLabel, onYes)` is the JS entry point (used by the terminal tab close).
- Replaced every `confirm()` / `window.confirm()` in templates (inbox card actions, agent remove, fleet launch/terminate, run cancel, task delete/ship, repo unlink, account remove, re-authorize) and `term.js`.
- `web/confirm_test.go` fails if a template or non-vendor script uses `confirm(` again. The e2e tests are HTTP-level and never handled browser dialogs, so none needed changes.
