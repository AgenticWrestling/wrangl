// cmd/ssm/internal/provision/provision.go
package provision

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

const script = `#!/bin/bash
set -euo pipefail

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
dnf install -y fish
usermod -s /usr/bin/fish ec2-user

echo "==> Installing Go"
dnf install -y golang

echo "==> Installing Rust"
if ! sudo -u ec2-user bash -c 'command -v rustup &>/dev/null'; then
  sudo -u ec2-user sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path'
else
  sudo -u ec2-user bash -c 'rustup update'
fi

echo "==> Installing Zed remote server"
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  | gunzip > /usr/local/bin/zed-remote
chmod +x /usr/local/bin/zed-remote
echo "Installed Zed remote server ${RELEASE} (${ARCH})"

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
playwright install chromium

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
