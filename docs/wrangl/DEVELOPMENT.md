# Developing wrangl

## Build from source

wrangl is one Go program (Go 1.26+, `CGO_ENABLED=0`, pure-Go SQLite), so it builds the same on
Linux and macOS. **Windows:** run the Linux build inside WSL (the `curl | sh` installer works in a WSL shell), or clone the repo and
compile there. A native Windows build is not tested.

```sh
git clone https://github.com/AgenticWrestling/wrangl && cd wrangl
go build -o wrangl ./cmd/wrangl        # for this machine
./wrangl version
```

[Task](https://taskfile.dev) wraps the common jobs (`task --list`):

```sh
task build-wrangl      # build/wrangl + build/wrangl-linux-{aarch64,x86_64}
task sqlc              # regenerate cmd/wrangl/internal/db/dbgen after editing SQL
task sqlc-check        # fail if dbgen is stale

build/wrangl --config ./c.yaml controller init --data-dir ./data
build/wrangl --config ./c.yaml controller serve
curl --unix-socket /run/wrangl/api.sock http://x/healthz

build/wrangl template create -f template.yaml
build/wrangl fleet launch --template go-claude     # tagged EC2 instance, enrols itself
build/wrangl fleet list
build/wrangl task tree my-project                  # the ranked backlog
build/wrangl task ask SQID "Any concerns?" --wait  # ask a controller agent
build/wrangl fleet teardown                        # terminate every tagged instance
```

| Task | What it does |
|---|---|
| `task build-wrangl` | `build/wrangl` for this machine plus `build/wrangl-linux-{aarch64,x86_64}` (the binaries the controller serves to runners) |
| `task test` | `go test ./...` |
| `task sqlc`, `task sqlc-check` | regenerate / verify the generated database code |
| `task prerelease` | release checks (see below) |
| `task release VERSION=v1.2.3` | cut a release |

## Releasing

Releases are binaries on GitHub for macOS (Apple silicon) and Linux (x86_64, arm64), installed by
`install.sh` (`curl -fsSL https://github.com/AgenticWrestling/wrangl/releases/latest/download/install.sh | sh`).

1. Write `docs/releases/vX.Y.Z.md` and merge it to `main` ([format](../releases/README.md)).
2. `task prerelease VERSION=vX.Y.Z`: clean `main` in sync with origin, `gofmt`, `go vet`, `go mod tidy`, sqlc, the full test suite, a build of every release target, the installer parses, the notes exist, and the tag is free.
3. `task release VERSION=vX.Y.Z`: repeats the checks, builds the three binaries with the version baked in, writes `checksums.txt`, shows what it will publish, and on confirmation tags, pushes and creates the GitHub release with the notes. `YES=1` skips the confirmation.

`scripts/prerelease.sh` and `scripts/release.sh` hold the logic, so they also run without Task.

**Web UI convention.** Client state lives in Datastar signals or HTML attributes, never in hand-written
JS, because every live update morphs `#main` from server HTML and clobbers anything the page set
itself. Use `data-preserve-attr="open"` on every `<details>` and modal `<dialog>`; `command="show-modal"`
/ `command="close"` with `commandfor` for sheets; `popover` / `popovertarget` for menus;
`data-signals`, `data-class`, `data-show`, `data-attr` and `data-bind` for toggles, selection and
form state (`data-class` re-applies itself after a morph; `data-show` does not, so use a class). No
inline `onclick`/`onchange`/`onsubmit`. Documented exceptions: the Back button, `enablePush`,
and `ghManifest` on the setup forms. The expanded task card is the `$open` signal (the title posts
`/ui/expand`, and the server puts that card in every live render); the lightbox is a `<dialog>` in
each gallery driven by `$lbi`. `static/app.js` keeps what has
no HTML equivalent: Sortable drag and drop, terminals, web push, the service worker, the
inline confirm, and the card's live output. `static/keys.js` is keyboard handling only (a chorded
list navigator does not fit `data-on:keydown__window`); the state it drives is signals. Each such
behaviour has a live-patch check in `cmd/wrangl/internal/e2e/browser/livepatch.js` (open the thing, inject a
probe into `#main`, cause an event, confirm the probe is gone and the state kept); run it with
`PLAYWRIGHT_DIR=<dir with node_modules/playwright> go test ./cmd/wrangl/internal/e2e -run TestLivePatch`.

The logo and icons are built from `docs/brand/source.jpg` by `docs/brand/make-logo.py`, and the
architecture diagram by `docs/brand/make-architecture.py`; see
[`docs/brand/`](../brand/README.md).
