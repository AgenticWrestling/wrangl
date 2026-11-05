# dwgTQS — Keyboard shortcuts for the desktop UI

Open `prototype.html#A`, `#B` or `#C` (append `-open` for the cheat sheet, `-chord` for C's "g" hint). The keys work in the prototype: `g p`, `1-5`, `n`, `c`, `/`, `?`, Esc.

Shared rules: keys are ignored while typing in an input/terminal, with Cmd/Ctrl/Alt held, and on touch devices. Plain digits pick sidebar recent projects; wterm's Cmd+digit (terminal tabs) is untouched, and the cheat sheet says so. Phones: no layout change, read-only list under Settings > Keyboard.

- **A — Modal cheat sheet**: always-visible key caps on sidebar rows and buttons; `?` opens a centered dialog.
- **B — Side drawer**: same visible caps; `?` opens a right drawer that leaves the page readable while you try keys.
- **C — Quiet UI + which-key**: no permanent caps (caps show on hover); `g` pops a small "Go to…" hint; `?` opens a compact corner card.

Heads-up: `keys.js` already binds `g p` = In progress, `c` = Complete (selected task) and `1-9` = projects. This task asks `g p` = Projects list and `c` = project chat, so those need remapping (e.g. In progress -> `g w`, Complete -> `C`/`Enter` on card) and 1-5 limits.

PNGs: `variant-<V>-<state>-{desktop,phone}.png`. Regenerate with `shoot.js`.
