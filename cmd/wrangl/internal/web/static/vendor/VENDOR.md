# Vendored front-end assets

Served from the binary (go:embed) so the PWA needs no CDN behind Cloudflare Access. Fetched from the npm registry on 2026-10-31; all MIT licensed.

| File | Package | sha256 |
|---|---|---|
| datastar-1.0.0-beta.11.js | @starfederation/datastar 1.0.0-beta.11 | fc47d2b4cfea9e6cdc93d287a0db38b2e894d813aa1e892e11de1be526620372 |
| sortable-1.15.7.min.js | sortablejs 1.15.7 | bf4241bc73fef7f11c59a283a69fe8051cdd31c6d8ff5a2b9ba219e7831fcf76 |
| xterm-6.0.0.js | @xterm/xterm 6.0.0 | 14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972 |
| xterm-addon-fit-0.11.0.js | @xterm/addon-fit 0.11.0 | ba3ea256ce0620a0992a197d6c9baea64823fc93d8da07a9e366ca9943c18527 |
| xterm-6.0.0.css | @xterm/xterm 6.0.0 | 854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6 |

## Fonts

Didact Gothic (SIL Open Font License 1.1), fetched from fonts.gstatic.com (Google Fonts, v21) on 2026-11-02. Only the latin and latin-ext subsets are served.

| File | sha256 |
|---|---|
| fonts/didact-gothic-latin.woff2 | a70246317c186a410244c8947aef7fedc04afe1da634eef3e6b2f37b72d5c627 |
| fonts/didact-gothic-latin-ext.woff2 | e2466a31de21630a2e8c181b7a919301dcaf4a1410ff9b2e1667639b744f6c20 |

IBM Plex Mono (SIL OFL 1.1), Google Fonts v20, latin subset, fetched 2026-11-02: fonts/ibm-plex-mono-latin.woff2 sha256 08949f728dc52d528e69b1667d15c89a5686a4ee9a296ff90983985f99c380f7

Icons: nine Font Awesome Free 6.5.2 solid SVGs (play, pen-to-square, code-branch, plus, hourglass-half, circle-check, ban, rotate-left, trash-can; CC BY 4.0) inlined as symbols in templates/layout.html.

## Web terminal (wterm)

wterm 0.5.7 (@wterm/dom + @wterm/core; Apache-2.0; https://github.com/vercel-labs/wterm), fetched from the npm registry on 2026-11-03. The JS is `esbuild --bundle --minify --format=iife --target=es2020` of `import * as wterm from "@wterm/dom"; window.wterm = wterm;` (its WASM core is inlined as base64, so no separate .wasm is served). The CSS is the package's unmodified `src/terminal.css`; the wrangl theme lives in `static/term.css`.

| File | sha256 |
|---|---|
| wterm-0.5.7.js | 5aed3b36eed47d1ec1c4e6696a1e509aae5e74fad4493a7ef29b693c6d402281 |
| wterm-0.5.7.css | d6963992474bef69242dd4e3b130619840c7954f34e8dbaa1f6d0035910bdd1b |
