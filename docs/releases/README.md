# Release notes

One file per release, named for the tag: `docs/releases/v0.1.0.md`. `task release VERSION=v0.1.0`
refuses to run without it, and publishes its text as the GitHub release notes.

Cutting a release:

```sh
task prerelease                # the checks, without a version (run any time)
$EDITOR docs/releases/v0.1.0.md
git add docs/releases && git commit -m "Release notes for v0.1.0" && git push
task prerelease VERSION=v0.1.0 # also checks the notes file and that the tag is free
task release VERSION=v0.1.0    # builds, asks to confirm, tags, publishes
```

`task release` builds `wrangl-darwin-aarch64`, `wrangl-linux-aarch64` and `wrangl-linux-x86_64`
(static, `CGO_ENABLED=0`) plus `checksums.txt` and `install.sh`, and uploads them. A version with a
suffix (`v0.2.0-rc.1`) is marked a prerelease, so `releases/latest` (and the `curl | sh` installer)
skips it; install one explicitly with `WRANGL_VERSION=v0.2.0-rc.1`.

A good note says what changed for someone running wrangl, in the order they care about:

```markdown
# wrangl v0.1.0

One-paragraph summary: what this release is.

## Highlights
- The two or three things worth upgrading for.

## Changes
- Added / changed / fixed, one line each, with the task or PR number.

## Upgrading
- Anything to do besides installing the new binary (migrations run themselves on controller start;
  say if a template, config key or Cloudflare/GitHub setting changes).

## Known issues
- What does not work yet.
```

Do not leave `TODO` or `FIXME` in the file: the prerelease check fails on them.
