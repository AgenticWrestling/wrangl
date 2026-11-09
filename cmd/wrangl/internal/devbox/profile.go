package devbox

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
)

// Profile is what `wrangl dev setup` applies: the two template fields that install software.
type Profile struct {
	MiseTOML    string `yaml:"mise_toml"`
	SetupScript string `yaml:"setup_script"`
}

// defaultTools is the default dev toolchain. The CLI tools are the ones a runner's first
// web-terminal shell installs (runner.ShellTools), so the two cannot drift apart.
const defaultTools = `# wrangl dev: default toolchain (applied with mise; edit a copy and pass it with --file)
[tools]
aws-cli = "latest"
go = "latest"
rust = "stable"
node = "lts"
python = "latest"
uv = "latest"
java = "corretto-25"
`

// defaultSetup is the whole OS-level residue: packages mise and ssh need, nothing else.
const defaultSetup = `if command -v dnf >/dev/null 2>&1; then
  dnf install -y git tar unzip gzip ca-certificates ec2-instance-connect || dnf install -y git tar unzip gzip ca-certificates
elif command -v apt-get >/dev/null 2>&1; then
  apt-get update -y
  DEBIAN_FRONTEND=noninteractive apt-get install -y git tar unzip gzip curl ca-certificates ec2-instance-connect || DEBIAN_FRONTEND=noninteractive apt-get install -y git tar unzip gzip curl ca-certificates
fi
`

// Default returns the embedded default profile.
func Default() Profile {
	return Profile{MiseTOML: defaultTools + strings.TrimPrefix(runner.ShellTools, "[tools]\n"), SetupScript: defaultSetup}
}

// overlays are optional additions selected with --with. Their tool names come from `mise
// registry`; the android and browser pieces are untested on a real instance (see ADR 0010).
var overlays = map[string]Profile{
	"android": {MiseTOML: "[tools]\njava = \"corretto-25\"\nandroid-sdk = \"latest\"\n"},
	"browser": {
		MiseTOML: "[tools]\n\"npm:playwright\" = \"latest\"\n",
		SetupScript: `if command -v dnf >/dev/null 2>&1; then
  dnf install -y atk at-spi2-atk at-spi2-core cups-libs libxcb libxkbcommon alsa-lib mesa-libgbm libX11 libXext libXcomposite libXdamage libXfixes libXrandr cairo pango
fi
`,
	},
	"fish": {MiseTOML: "[tools]\n\"github:fish-shell/fish-shell\" = \"latest\"\n"},
}

// OverlayNames lists the valid --with values.
func OverlayNames() []string { return []string{"android", "browser", "fish"} }

// Merge appends the named overlays: their [tools] tables are merged and their setup scripts
// run after the profile's.
func (p Profile) Merge(names ...string) (Profile, error) {
	for _, n := range names {
		o, ok := overlays[n]
		if !ok {
			return p, fmt.Errorf("unknown overlay %q (have %s)", n, strings.Join(OverlayNames(), ", "))
		}
		p.MiseTOML = mergeTools(p.MiseTOML, o.MiseTOML)
		if o.SetupScript != "" {
			p.SetupScript = strings.TrimRight(p.SetupScript, "\n") + "\n" + o.SetupScript
		}
	}
	return p, nil
}

// mergeTools appends the extra config to base: a second [tools] header is dropped so the
// entries join the first table (TOML forbids defining a table twice) and a key the base
// already sets is skipped (TOML forbids duplicate keys), so the base's version wins.
func mergeTools(base, extra string) string {
	keyOf := func(l string) string {
		k, _, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(l), "#") {
			return ""
		}
		return strings.TrimSpace(k)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(base, "\n") {
		if k := keyOf(l); k != "" {
			have[k] = true
		}
	}
	var out []string
	for _, l := range strings.Split(extra, "\n") {
		if strings.TrimSpace(l) == "[tools]" && strings.Contains(base, "[tools]") {
			continue
		}
		if k := keyOf(l); k != "" && have[k] {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimRight(base, "\n") + "\n" + strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

// LoadFile reads a profile from a YAML file with the keys mise_toml and setup_script (the
// same names a runner template uses, so a template file works too).
func LoadFile(path string) (Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(p.MiseTOML) == "" && strings.TrimSpace(p.SetupScript) == "" {
		return p, fmt.Errorf("%s: neither mise_toml nor setup_script is set", path)
	}
	return p, nil
}

// Apply converts a profile to the value ApplyScript renders.
func (p Profile) Apply() proto.TemplateApply {
	return proto.TemplateApply{MiseTOML: p.MiseTOML, SetupScript: p.SetupScript}
}

// IdleStopScript returns a setup_script fragment installing a systemd timer that powers the
// machine off when it has been idle (CPU below 5% and nobody logged in) for d. Powering off
// stops an EBS-backed EC2 instance, so no AWS CLI or instance-profile permission is needed.
func IdleStopScript(d time.Duration) string {
	return fmt.Sprintf(`install -d /var/lib/wrangl-dev
cat > /usr/local/bin/wrangl-idle-stop <<'IDLEEOF'
#!/bin/bash
# Power off after %[1]d seconds without load. Busy = CPU >= 5%% over 5 s, or a logged-in user.
STAMP=/var/lib/wrangl-dev/last-active
cpu() {
  read -r _ u n s i w q sq st _ < /proc/stat; t1=$((u+n+s+i+w+q+sq+st)); i1=$i
  sleep 5
  read -r _ u n s i w q sq st _ < /proc/stat; t2=$((u+n+s+i+w+q+sq+st)); i2=$i
  echo $(( ($t2-$t1-($i2-$i1)) * 100 / ($t2-$t1) ))
}
if [ "$(cpu)" -ge 5 ] || [ -n "$(who)" ]; then date +%%s > "$STAMP"; exit 0; fi
[ -f "$STAMP" ] || date +%%s > "$STAMP"
if [ $(( $(date +%%s) - $(cat "$STAMP") )) -ge %[1]d ]; then
  logger -t wrangl-idle-stop "idle for %[1]ds, powering off"
  systemctl poweroff
fi
IDLEEOF
chmod +x /usr/local/bin/wrangl-idle-stop
cat > /etc/systemd/system/wrangl-idle-stop.service <<'IDLEEOF'
[Unit]
Description=Power off when idle
[Service]
Type=oneshot
ExecStart=/usr/local/bin/wrangl-idle-stop
IDLEEOF
cat > /etc/systemd/system/wrangl-idle-stop.timer <<'IDLEEOF'
[Unit]
Description=Check for idleness every 10 minutes
[Timer]
OnBootSec=10min
OnUnitActiveSec=10min
[Install]
WantedBy=timers.target
IDLEEOF
systemctl daemon-reload
systemctl enable --now wrangl-idle-stop.timer
`, int(d.Seconds()))
}
