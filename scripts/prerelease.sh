#!/usr/bin/env bash
# Prerelease checks. With a VERSION (vX.Y.Z[-suffix]) it also checks the release inputs:
# the notes file and that the tag is free. Run via `task prerelease [VERSION=v0.1.0]`.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-}"
fail=0
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }
warn() { printf '  warn  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

step "git"
branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = main ] && ok "on main" || bad "on branch $branch, releases come from main"
[ -z "$(git status --porcelain)" ] && ok "working tree clean" || bad "uncommitted changes"
if git fetch -q origin main 2>/dev/null; then
  [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] && ok "in sync with origin/main" || bad "HEAD differs from origin/main (push or pull first)"
else
  bad "cannot reach origin"
fi

step "code"
unformatted="$(gofmt -l cmd 2>/dev/null || true)"
[ -z "$unformatted" ] && ok "gofmt" || bad "gofmt needed: $unformatted"
go vet ./... >/dev/null 2>&1 && ok "go vet" || bad "go vet"
if go mod tidy -diff >/dev/null 2>&1; then ok "go.mod tidy"; else bad "go.mod/go.sum not tidy (run go mod tidy)"; fi
if command -v sqlc >/dev/null 2>&1; then
  (cd cmd/wrangl/internal/db && sqlc diff >/dev/null 2>&1) && ok "sqlc generated code current" || bad "sqlc code is stale (task sqlc)"
else
  warn "sqlc not installed: skipped the generated-code check"
fi
go test ./... >/tmp/wrangl-prerelease-test.log 2>&1 && ok "go test ./..." || { bad "go test ./... (see /tmp/wrangl-prerelease-test.log)"; tail -15 /tmp/wrangl-prerelease-test.log; }

step "release targets build"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
for t in darwin/arm64 linux/amd64 linux/arm64; do
  if CGO_ENABLED=0 GOOS="${t%/*}" GOARCH="${t#*/}" go build -trimpath -o "$tmp/wrangl-${t%/*}-${t#*/}" ./cmd/wrangl 2>"$tmp/err"; then ok "build $t"; else bad "build $t"; head -5 "$tmp/err"; fi
done

step "installer"
sh -n install.sh && ok "install.sh parses" || bad "install.sh syntax"
if command -v shellcheck >/dev/null 2>&1; then shellcheck -s sh install.sh >/dev/null && ok "shellcheck install.sh" || bad "shellcheck install.sh"; else warn "shellcheck not installed"; fi

if [ -n "$VERSION" ]; then
  step "release $VERSION"
  [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] && ok "version format" || bad "VERSION must look like v1.2.3 or v1.2.3-rc.1"
  notes="docs/releases/$VERSION.md"
  if [ -s "$notes" ]; then
    ok "release notes $notes"
    grep -q 'TODO\|FIXME' "$notes" && bad "$notes still has TODO/FIXME" || true
  else
    bad "missing release notes: write $notes (see docs/releases/README.md)"
  fi
  git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null && bad "tag $VERSION already exists" || ok "tag $VERSION is free"
  command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1 && ok "gh authenticated" || bad "gh missing or not logged in"
fi

echo
if [ "$fail" = 0 ]; then echo "prerelease checks passed"; else echo "prerelease checks FAILED"; exit 1; fi
