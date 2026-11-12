#!/usr/bin/env bash
# Cut a release: run the prerelease checks, build binaries for macOS (arm64) and Linux
# (x86_64, arm64), tag, and publish a GitHub release with docs/releases/<VERSION>.md as
# its notes. Run via `task release VERSION=v0.1.0` (YES=1 skips the confirmation; DRY_RUN=1 builds everything but tags and publishes nothing).
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-}"
[ -n "$VERSION" ] || { echo "usage: task release VERSION=v1.2.3" >&2; exit 2; }
NOTES="docs/releases/$VERSION.md"

if ! scripts/prerelease.sh "$VERSION"; then
  [ "${DRY_RUN:-}" = 1 ] || exit 1
  echo "(DRY_RUN: continuing despite failed checks)"
fi

dist="dist/$VERSION"
rm -rf "$dist"; mkdir -p "$dist"
LD="-s -w -X github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner.Version=$VERSION"
echo; echo "== building $VERSION"
for t in darwin/arm64:wrangl-darwin-aarch64 linux/arm64:wrangl-linux-aarch64 linux/amd64:wrangl-linux-x86_64; do
  plat="${t%%:*}"; out="${t##*:}"
  CGO_ENABLED=0 GOOS="${plat%/*}" GOARCH="${plat#*/}" go build -trimpath -ldflags "$LD" -o "$dist/$out" ./cmd/wrangl
  echo "  $out"
done
cp install.sh "$dist/install.sh"
(cd "$dist" && { command -v sha256sum >/dev/null && sha256sum wrangl-* || shasum -a 256 wrangl-*; } > checksums.txt)

# The binary for this machine must run and report the version.
host="wrangl-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/arm64/aarch64/')"
if [ -x "$dist/$host" ]; then
  got="$("$dist/$host" version)"
  case "$got" in "wrangl $VERSION "*) echo "  smoke test: $got" ;; *) echo "smoke test failed: $got" >&2; exit 1 ;; esac
fi

pre=""; case "$VERSION" in *-*) pre="--prerelease" ;; esac
echo; echo "== about to publish"
echo "  tag      $VERSION on $(git rev-parse --short HEAD)"
echo "  assets   $(ls "$dist" | tr '\n' ' ')"
echo "  notes    $NOTES"
sed 's/^/    | /' "$NOTES" | head -20
if [ "${DRY_RUN:-}" = 1 ]; then echo "DRY_RUN: built in $dist, nothing tagged or published"; exit 0; fi
if [ "${YES:-}" != 1 ]; then
  read -r -p "Tag, push and publish $VERSION? [y/N] " a
  [ "$a" = y ] || [ "$a" = Y ] || { echo "aborted; binaries are in $dist"; exit 1; }
fi

git tag -a "$VERSION" -m "wrangl $VERSION"
git push origin "$VERSION"
# shellcheck disable=SC2086
gh release create "$VERSION" "$dist"/* --title "wrangl $VERSION" --notes-file "$NOTES" $pre
echo; echo "published: $(gh release view "$VERSION" --json url -q .url)"
echo "install:   curl -fsSL https://github.com/AgenticWrestling/wrangl/releases/latest/download/install.sh | sh"
