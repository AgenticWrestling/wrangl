#!/bin/sh
# wrangl installer: downloads the wrangl binary for this machine from a GitHub release,
# checks its SHA-256, and puts it on your PATH.
#
#   curl -fsSL https://github.com/AgenticWrestling/wrangl/releases/latest/download/install.sh | sh
#
# Environment:
#   WRANGL_VERSION      release tag to install (default: the latest release), e.g. v0.2.0
#   WRANGL_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#
# Prebuilt binaries: macOS on Apple silicon, Linux on x86_64 and arm64. On Windows use WSL
# (the Linux installer works there) or build from source: see docs/wrangl/DEVELOPMENT.md.
set -eu

REPO="AgenticWrestling/wrangl"
DIR="${WRANGL_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${WRANGL_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  MINGW*|MSYS*|CYGWIN*) die "Windows is not supported by this installer. Use WSL (then re-run this in the WSL shell), or build from source: https://github.com/$REPO/blob/main/docs/wrangl/DEVELOPMENT.md" ;;
  *) die "unsupported OS $(uname -s); build from source: https://github.com/$REPO/blob/main/docs/wrangl/DEVELOPMENT.md" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=x86_64 ;;
  arm64|aarch64) arch=aarch64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac
if [ "$os-$arch" = "darwin-x86_64" ]; then
  die "no prebuilt binary for Intel Macs; build from source: https://github.com/$REPO/blob/main/docs/wrangl/DEVELOPMENT.md"
fi

asset="wrangl-$os-$arch"
if [ -n "${WRANGL_BASE_URL:-}" ]; then  # for testing the installer against a local server
  base="$WRANGL_BASE_URL"
elif [ "$VERSION" = "latest" ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q "$1" -O "$2"; }
else
  die "need curl or wget"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $asset ($VERSION)..."
fetch "$base/$asset" "$tmp/$asset" || die "could not download $base/$asset (does that release exist?)"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download checksums.txt"

want="$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  got="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
else
  die "need sha256sum or shasum to verify the download"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $asset (expected $want, got $got); not installing"

mkdir -p "$DIR"
chmod +x "$tmp/$asset"
mv "$tmp/$asset" "$DIR/wrangl.new"
mv "$DIR/wrangl.new" "$DIR/wrangl"

say "Installed $("$DIR/wrangl" version 2>/dev/null || echo wrangl) to $DIR/wrangl"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) say "Note: $DIR is not on your PATH. Add it, for example:"
     say "  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.profile" ;;
esac
say "Next: wrangl --help   (setup guide: https://github.com/$REPO/blob/main/docs/wrangl/INSTALL.md)"
