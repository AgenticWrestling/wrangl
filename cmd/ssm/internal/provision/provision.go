// cmd/ssm/internal/provision/provision.go
package provision

import (
	"context"
	"fmt"

	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const script = `#!/bin/bash
set -euxo pipefail

ARCH=$(uname -m)

echo "==> Installing AWS CLI v2 (${ARCH})"
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-${ARCH}.zip" -o /tmp/awscliv2.zip
unzip -qo /tmp/awscliv2.zip -d /tmp
if [ -d /usr/local/aws-cli ]; then
  /tmp/aws/install --update
else
  /tmp/aws/install
fi
rm -rf /tmp/aws /tmp/awscliv2.zip

echo "==> Installing ec2-instance-connect"
dnf install -y ec2-instance-connect

echo "==> Installing fish shell"
FISH_VERSION=$(curl -sf https://api.github.com/repos/fish-shell/fish-shell/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/fish-shell/fish-shell/releases/download/${FISH_VERSION}/fish-${FISH_VERSION}-linux-${ARCH}.tar.xz" \
  -o /tmp/fish.tar.xz
tar -xJ -O -f /tmp/fish.tar.xz > /usr/local/bin/fish
rm /tmp/fish.tar.xz
chmod +x /usr/local/bin/fish
grep -qxF /usr/local/bin/fish /etc/shells || echo /usr/local/bin/fish >> /etc/shells
usermod -s /usr/local/bin/fish ec2-user

echo "==> Installing Go"
dnf install -y golang

echo "==> Installing Rust"
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs -o /tmp/rustup-init.sh
if ! sudo -u ec2-user bash -c 'command -v rustup &>/dev/null'; then
  sudo -u ec2-user sh /tmp/rustup-init.sh -y --no-modify-path >/tmp/rustup.log 2>&1
else
  sudo -u ec2-user bash -c 'rustup update >/tmp/rustup.log 2>&1'
fi
rm /tmp/rustup-init.sh
cat /tmp/rustup.log

echo "==> Installing Zed remote server"
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  -o /tmp/zed-remote.gz
gunzip -c /tmp/zed-remote.gz > /usr/local/bin/zed-remote
rm /tmp/zed-remote.gz
chmod +x /usr/local/bin/zed-remote
echo "Installed Zed remote server ${RELEASE} (${ARCH})"

echo "==> Creating njr user"
if ! id njr &>/dev/null; then
  useradd -m -s /usr/local/bin/fish njr
fi
usermod -aG wheel njr
mkdir -p /home/njr/.ssh
chmod 700 /home/njr/.ssh
cp /home/ec2-user/.ssh/authorized_keys /home/njr/.ssh/authorized_keys
chmod 600 /home/njr/.ssh/authorized_keys
chown -R njr:njr /home/njr/.ssh

echo "==> Installing Java 25 (Amazon Corretto)"
rpm --import https://yum.corretto.aws/corretto.key
curl -fsSL -o /etc/yum.repos.d/corretto.repo https://yum.corretto.aws/corretto.repo
dnf install -y java-25-amazon-corretto-headless

echo "==> Installing Android command-line tools"
ANDROID_HOME=/opt/android-sdk
mkdir -p "${ANDROID_HOME}/cmdline-tools"
curl -fsSL "https://dl.google.com/android/repository/commandlinetools-linux-14742923_latest.zip" \
  -o /tmp/cmdline-tools.zip
unzip -qo /tmp/cmdline-tools.zip -d /tmp/cmdtools
rm -rf "${ANDROID_HOME}/cmdline-tools/latest"
mv /tmp/cmdtools/cmdline-tools "${ANDROID_HOME}/cmdline-tools/latest"
rm -rf /tmp/cmdtools /tmp/cmdline-tools.zip

echo "==> Accepting Android SDK licenses"
(yes || true) | "${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager" --licenses

echo "==> Installing Android SDK platforms and tools"
"${ANDROID_HOME}/cmdline-tools/latest/bin/sdkmanager" \
  "platform-tools" \
  "build-tools;35.0.0" \
  "platforms;android-35" \
  "platforms;android-34"

echo "==> Installing Android CLI"
curl -fsSL "https://dl.google.com/android/cli/latest/linux_x86_64/android" \
  -o /usr/local/bin/android
chmod +x /usr/local/bin/android

echo "==> Setting Android SDK ownership and fish environment for njr"
chown -R njr:njr "${ANDROID_HOME}"
mkdir -p /home/njr/.config/fish
cat > /home/njr/.config/fish/config.fish <<'FISHEOF'
# Android SDK
set -x ANDROID_HOME /opt/android-sdk
set -x PATH $PATH $ANDROID_HOME/cmdline-tools/latest/bin $ANDROID_HOME/platform-tools $ANDROID_HOME/build-tools/35.0.0

# Java
set -x JAVA_HOME (dirname (dirname (readlink -f (which java))))
set -x PATH $PATH $JAVA_HOME/bin

# Android CLI
set -x PATH $PATH /usr/local/bin
FISHEOF
chown -R njr:njr /home/njr/.config

echo "==> Installing Python Playwright"
dnf install -y python3-pip
pip3 install playwright
# playwright install-deps only supports apt; install Chromium deps manually
dnf install -y \
  atk at-spi2-atk at-spi2-core \
  cups-libs \
  libxcb libxkbcommon \
  alsa-lib \
  mesa-libgbm \
  libX11 libXext libXcomposite libXdamage libXfixes libXrandr \
  cairo pango
python3 -m playwright install chromium

echo "==> Installing idle auto-stop (4h CPU idle threshold)"
cat > /usr/local/bin/devhost-idle-stop <<'STOPEOF'
#!/bin/bash
# Stop this instance if CPU has been idle for 4 consecutive hours.
# Reads /proc/stat to compute CPU busy percentage over a 60s window.
# Writes last-active timestamp to /tmp/devhost-last-active when busy.
IDLE_THRESHOLD_SECONDS=$((1 * 3600))
BUSY_THRESHOLD_PCT=5
STAMP=/tmp/devhost-last-active

cpu_pct() {
  read -r _ user nice system idle iowait irq softirq steal _ < /proc/stat
  total1=$((user+nice+system+idle+iowait+irq+softirq+steal))
  idle1=$idle
  sleep 5
  read -r _ user nice system idle iowait irq softirq steal _ < /proc/stat
  total2=$((user+nice+system+idle+iowait+irq+softirq+steal))
  idle2=$idle
  dtotal=$((total2-total1))
  didle=$((idle2-idle1))
  echo $(( (dtotal - didle) * 100 / dtotal ))
}

pct=$(cpu_pct)
if [ "$pct" -ge "$BUSY_THRESHOLD_PCT" ]; then
  date +%s > "$STAMP"
  exit 0
fi

[ -f "$STAMP" ] || date +%s > "$STAMP"
last=$(cat "$STAMP")
now=$(date +%s)
idle_for=$(( now - last ))

if [ "$idle_for" -ge "$IDLE_THRESHOLD_SECONDS" ]; then
  INSTANCE_ID=$(curl -sf http://169.254.169.254/latest/meta-data/instance-id)
  REGION=$(curl -sf http://169.254.169.254/latest/meta-data/placement/region)
  logger -t devhost-idle-stop "CPU idle for ${idle_for}s, stopping ${INSTANCE_ID}"
  aws ec2 stop-instances --region "$REGION" --instance-ids "$INSTANCE_ID"
fi
STOPEOF
chmod +x /usr/local/bin/devhost-idle-stop

# Run every 10 minutes via systemd timer
cat > /etc/systemd/system/devhost-idle-stop.service <<'SVCEOF'
[Unit]
Description=Stop instance if CPU idle for 4h

[Service]
Type=oneshot
ExecStart=/usr/local/bin/devhost-idle-stop
SVCEOF

cat > /etc/systemd/system/devhost-idle-stop.timer <<'TIMEREOF'
[Unit]
Description=Check CPU idle every 10 minutes

[Timer]
OnBootSec=10min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
TIMEREOF

systemctl daemon-reload
systemctl enable --now devhost-idle-stop.timer

echo "==> All done"
`

// Run sends the dev toolchain install script to the instance via SSM send-command.
// Returns the SSM Command ID.
func Run(ctx context.Context, profile, region, instanceID string) (commandID string, err error) {
	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return "", err
	}
	client := ssm.NewFromConfig(cfg)

	out, err := client.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceID},
		Parameters: map[string][]string{
			"commands": {script},
		},
		TimeoutSeconds: aws.Int32(3600),
		CloudWatchOutputConfig: &ssmtypes.CloudWatchOutputConfig{
			CloudWatchLogGroupName:  aws.String("/aws/ssm/AWS-RunShellScript"),
			CloudWatchOutputEnabled: true,
		},
	})
	if err != nil {
		return "", fmt.Errorf("send-command: %w", err)
	}
	return aws.ToString(out.Command.CommandId), nil
}
