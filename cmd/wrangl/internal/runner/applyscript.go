package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// ScriptOptions tune ApplyScript. The zero value is what `wrangl dev setup` runs as root on a
// real machine; the fields exist so a test can run the script unprivileged in a temp dir.
type ScriptOptions struct {
	MiseBin  string // default /usr/local/bin/mise
	StateDir string // default /var/lib/wrangl-dev
	Home     string // the user's home; default: from the passwd database
	ConfName string // file in ~/.config/mise/conf.d; default wrangl-dev.toml
	NoSudo   bool   // run mise as the current user instead of `sudo -u USER`
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// heredocTag returns a delimiter that does not occur in body.
func heredocTag(body string) string {
	sum := sha256.Sum256([]byte(body))
	tag := "WRANGL_EOF_" + hex.EncodeToString(sum[:4])
	for strings.Contains(body, tag) {
		tag += "_X"
	}
	return tag
}

// ApplyScript renders a self-contained bash script that does on a remote machine what
// Applier.Apply does on a runner: install mise if missing (the same mise.run installer),
// run the setup script as root when its content changed, write the mise config for user
// and run `mise install` as that user. It is idempotent. Unlike Apply it writes the config to
// ~/.config/mise/conf.d, so a machine's own config.toml (or a runner's template) is left alone.
func ApplyScript(t proto.TemplateApply, user string, o ScriptOptions) string {
	if o.MiseBin == "" {
		o.MiseBin = DefaultMiseBin
	}
	if o.StateDir == "" {
		o.StateDir = "/var/lib/wrangl-dev"
	}
	if o.ConfName == "" {
		o.ConfName = "wrangl-dev.toml"
	}
	sum := sha256.Sum256([]byte(t.SetupScript))
	setupTag, confTag := heredocTag(t.SetupScript), heredocTag(t.MiseTOML)
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("MISE=%s", shQuote(o.MiseBin))
	w("STATE=%s", shQuote(o.StateDir))
	w("TARGET_USER=%s", shQuote(user))
	if o.Home != "" {
		w("TARGET_HOME=%s", shQuote(o.Home))
	} else {
		w(`TARGET_HOME=$(getent passwd "$TARGET_USER" | cut -d: -f6)`)
		w(`[ -n "$TARGET_HOME" ] || { echo "no such user: $TARGET_USER" >&2; exit 1; }`)
	}
	w(`mkdir -p "$STATE"`)
	w(`if [ ! -x "$MISE" ]; then`)
	w(`  echo "==> installing mise to $MISE"`)
	w(`  inst=$(mktemp)`)
	w(`  curl -fsSL --retry 3 -o "$inst" https://mise.run`)
	w(`  MISE_INSTALL_PATH="$MISE" sh "$inst"`)
	w(`  rm -f "$inst"`)
	w(`fi`)
	w(`if [ "$(cat "$STATE/setup.sha256" 2>/dev/null || true)" != %s ]; then`, shQuote(hex.EncodeToString(sum[:])))
	if strings.TrimSpace(t.SetupScript) != "" {
		w(`  echo "==> running setup script"`)
		w(`  setup=$(mktemp)`)
		w("  cat > \"$setup\" <<'%s'", setupTag)
		b.WriteString(t.SetupScript)
		if !strings.HasSuffix(t.SetupScript, "\n") {
			b.WriteString("\n")
		}
		w("%s", setupTag)
		w(`  (cd / && bash -euo pipefail "$setup")`)
		w(`  rm -f "$setup"`)
	}
	w(`  echo %s > "$STATE/setup.sha256"`, hex.EncodeToString(sum[:]))
	w(`else`)
	w(`  echo "==> setup script unchanged; skipping"`)
	w(`fi`)
	w(`echo "==> writing mise config for $TARGET_USER"`)
	w(`confdir="$TARGET_HOME/.config/mise/conf.d"`)
	w(`mkdir -p "$confdir"`)
	w("cat > \"$confdir/%s\" <<'%s'", o.ConfName, confTag)
	b.WriteString(t.MiseTOML)
	if !strings.HasSuffix(t.MiseTOML, "\n") {
		b.WriteString("\n")
	}
	w("%s", confTag)
	if o.NoSudo {
		w(`echo "==> mise install (as $TARGET_USER)"`)
		w(`cd "$TARGET_HOME" && "$MISE" install --yes`)
	} else {
		w(`chown -R "$TARGET_USER": "$TARGET_HOME/.config"`)
		w(`echo "==> mise install (as $TARGET_USER)"`)
		w(`cd "$TARGET_HOME"`)
		w(`sudo -u "$TARGET_USER" -H env HOME="$TARGET_HOME" PATH="$(dirname "$MISE"):/usr/local/bin:/usr/bin:/bin" MISE_YES=1 "$MISE" install --yes`)
	}
	w(`echo "==> done"`)
	return b.String()
}
