# VyfcV5 — Custom tools UI: Settings › Tools, project "Custom tools" card, run-page audit

Interactive prototype: open `prototype.html#<variant>-<screen>` (variants `A`, `B`, `C`; screens `settings`, `editor`, `project`, `run`; add `-bad` to the editor for validation errors, `-del` on settings for the delete confirm). The small switcher at the top right changes variant and screen. It is clickable: enable/disable a tool, paste a secret and Save (the field is write-only: it flips to "✓ set" and never shows the value again), Validate/Save the KDL, delete with confirmation. No Claude Artifact link (the artifact tool is not available here); the HTML is self-contained next to the images.

Images: `variant-<V>-<n>-<screen>-{desktop,phone}.png` (1280 wide, and 390 wide at 2x). Regenerate with `shoot.js`. They use the app's real tokens (dark palette, Red Hat Display, IBM Plex Mono, pills, tags, sidebar/tab bar). Icons are inline SVG in the mockup; the app would use its own glyph set.

## Shared rules (all variants)
- **One library, per-project enablement** (decision 8). Settings › Tools owns definitions; the project page can only enable/disable and set that project's secrets, never edit a definition. The project card links to the library.
- **Status never relies on colour alone**: effect badges are `read` (eye), `write` (pencil), `destructive` (warning triangle, with "asks you before every call" in the detail); secrets are `✓ set` / `! missing`; audit statuses carry a glyph and a word.
- **Secret field is write-only.** Shown only when missing or after "Replace"; helper line names the store key (`tool.<project>.<name>`), notes when another enabled tool shares the secret, and gives the CLI equivalent.
- **Review and plan runs only get read tools** is stated on the tool detail, where a non-read tool's run kinds are shown.
- **Validation errors are a list under the editor**, not inline line markers: the KDL wrapper does not return positions today, so the mockup does not pretend to highlight lines. (Adding positions to `internal/kdl` would be a follow-up for line markers.)
- **Run page audit** shows one row per call: tool, status, duration, bytes, when. It says arguments and output are not stored (true: the audit row holds none). Truncated results are flagged.

## Variants
- **A — Table + stacked cards (closest to Setup).** Settings › Tools is a table (tool, kind, effect, run kinds, enabled on) with the selected tool's detail card below; the editor is a single full-width card. Project card: one sub-card per enabled tool with its secrets inline, an "Enable a tool" menu for the rest. Run page: a plain table. Phone: the table collapses to stacked rows. Smallest change from existing patterns.
- **B — Master/detail + whole-library checklist.** Settings › Tools is a list on the left, detail on the right; the editor has a side panel "What agents will see" (the declaration only; argv, URLs and secret names hidden) and "Needs". Project card: every library tool as a row with a switch, so you see what you are *not* using; enabled rows expand their secrets in the same table. Run page: a timeline with a per-tool count strip. Most informative on desktop; on phones the lists stack.
- **C — Phone-first, attention-first.** Settings › Tools is cards. "Add tool" starts from a starting-point picker (HTTP request now; Command and Remote MCP shown as "not yet" until phases 2–3) before the KDL editor. Project card leads with a **missing-secrets block** (set them right there), then compact toggles, then "Not enabled" chips to add. Run page: collapsed summary (✓ ok / ! not ok / … awaiting you) that expands to rows; a pending destructive call links to the Inbox approval.

## Recommendation
A for the Settings page and editor (matches the rest of Settings and costs least), with C's missing-secrets-first block on the project card (it answers "why did my tool call fail" fastest), and B's "What agents will see" panel as a collapsible in the editor. Run page: A's table on desktop, C's collapsed summary on phones.

## Open questions for the owner
1. Settings has one long page of cards today: is a separate `/settings/tools` page (as drawn) right, or a card with an inline list?
2. Should enabling a tool with missing secrets be allowed (drawn: yes, with a "missing" warning, matching the API) or blocked?
3. "Test…" on a tool (really calls the upstream, as the owner): confirm button wording and whether destructive tools can be tested at all.
