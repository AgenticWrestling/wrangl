# Gacb0D — Project chat UI (design + implementation)

Prototype: `prototype.html` (`#A`, `#A-closed`, `#B`, `#C`; resize for phone). Screenshots: `variant-{A,A-closed,B,C}-{desktop,phone}.png`; `node shoot.js` regenerates them. In the screenshots monospace task ids and icon glyphs are blank/boxes: the headless browser had no monospace or symbol font.

| | Desktop | Phone | Trade-off |
|---|---|---|---|
| A (implemented) | 400px right-hand panel; the page shifts left and stays usable | full-screen chat; floating 💬 button when closed (above the tab bar) | one pattern, all the room for proposals; hides the page while open, so a task link closes the chat and flashes its row |
| B | same panel | half-height bottom sheet over the page | page stays visible, but the keyboard leaves a sliver for the log |
| C | inline card in the page, above the sections | same inline card | no overlay at all, but scrolls away and competes with the task lists |

Recommendation: A.

## Implemented
- **Comment** button next to New task opens the chat (`$chat` signal); the Projects list's Comment action links to `/projects/{slug}?chat=1`, which opens it on load.
- Header with a context chip ("N tasks · N runs · N PRs · N repos [· trimmed]") from the backend's brief; messages like the task conversation; proposals render as **Approve: <label>** buttons (done = ✓, failed = ! and the reason); input accepts Ctrl/Cmd+Enter and `/queue` / `/dequeue`.
- Task ids in agent replies (in backticks) become links: on this page they scroll to the row and flash it (closing the full-screen chat on a phone first); otherwise they open the task.
- Acts: `project-ask`, `project-proposal-apply`. The chat re-renders with the page's live stream.
- e2e: `TestProjectChatInBrowser` (`e2e/chat_browser_test.go`, `e2e/browser/chat.js`) drives headless Chromium at 1280x800 and 390x844 against a real controller: chat closed by default, floating button only on the phone, right-hand panel vs full screen geometry, context chip, `/queue` ambiguity → Approve buttons → approve, a linked task id → flash (and the chat closing on the phone). It needs node and Playwright: it runs only when `PLAYWRIGHT_DIR` is set (e.g. a directory whose node_modules has playwright, with chromium installed) and is skipped otherwise.
