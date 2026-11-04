# ykpdKE — Transcript as Markdown in the expanded task card (design)

Interactive prototype: `prototype.html` (open it; `#A`, `#B`, `#C` pick a variant; the top bar switches state — live / finished / no transcript — appends a live entry, or scrolls the panel up so you can see the "paused" behaviour). Fonts are in `fonts/`. No application code was changed.

`shoot.js` regenerates the PNGs (`variant-{A,B,C}-{live,paused,thinking,done,none}-{desktop,phone}.png`). Desktop is the card at 1100px; phone is 390px @2x.

## Shared across variants (the contract the build should follow)
- The panel replaces `.liveterm`. It keeps `data-ignore-morph`, a fixed max height (25rem desktop, 30rem phone) and its own scroll, so the page never jumps.
- **Assistant text** is prose: server-side goldmark, raw HTML off. The malicious `<script>` in the fixture shows as inert code text (see the "All green…" message).
- **Tool calls** are one-line chips: `Bash: go test ./...` plus a status glyph (`✓` green / `✗` vermillion / `…` pulsing while running). Status is never colour alone. A `tool_use` and its `tool_result` are one chip: the result patches the chip by id (`tool-<seq>`), it does not add a row. Click expands a short output excerpt (truncated, with "full output on the run page"). Expanded state is client-held (`<details>`-style, no JS beyond what Datastar already gives) so live patches that append do not collapse it.
- **Thinking** is hidden by default; a `▸ thinking` toggle in the panel header reveals it (dashed left rule, italic, dim).
- **Follow the tail**: pinned to the bottom while the user is at the bottom. Scrolling up pauses following (a status shows "Paused"); new entries keep arriving and a `↓ N new` pill (A/B) or status-bar link (C) jumps back down and resumes.
- **Load earlier** is a link at the top of the list; it prepends older entries (`?before=seq` JSON tail, rendered server-side) and keeps the scroll position.
- **Fallback**: no transcript (non-Claude agent / old run) → today's PTY view under an amber note explaining why. **Finished** runs show the stored entries with no live dot and no pill.

| Variant | Idea | Trade-off |
|---|---|---|
| A Chat log | Prose flows like a chat; consecutive tool calls wrap as pill chips; expanding a chip opens its excerpt under the row | Most familiar, least chrome; many tool calls make a long chip cloud |
| B Timeline rail | Time gutter + rail; tool calls are full-width rows (tool, arg, duration, status) | Best for scanning "what happened when" and spotting the failing step; costs ~54px of gutter (narrowed to 34px on phone) |
| C Reader with filter | Header segmented control Prose / + Tools / All; consecutive tool calls collapse to "▸ 2 tool calls Edit, Bash ✗ 1 failed"; a status bar says Following / Paused | Best for reading the agent's reasoning and for phones; tool detail is one extra tap away; most UI to build |

## Recommendation
C's collapsed tool groups and status bar as the default (the owner mostly wants the story, and failures stay visible via "✗ 1 failed"), with B's row layout used for the expanded group and B's timestamps on desktop. A's pill chips are the cheapest first build if we want to ship the smallest PR.

## Notes for implementation
- Datastar fit: entries append via `patch-elements` with `mode append` onto `#tl-<run>`; chip updates are `patch-elements` by id (`tool-<seq>`). The expanded excerpt lives inside the chip element, so patch the chip with the open state preserved (or use native `<details>` and keep `open` on the morph — the card is `data-ignore-morph`, so only explicit patches touch it).
- Following/paused is the only client logic: one scroll listener toggling a `data-following` attribute; appended patches scroll only when it is set.
- Prototype quirk: it only simulates; the real excerpts are the runner-truncated 400-char strings.
