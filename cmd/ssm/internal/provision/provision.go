// cmd/ssm/internal/provision/provision.go
package provision

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const script = `#!/bin/bash
set -euo pipefail

echo "==> Installing AWS CLI v2"
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip && unzip -q /tmp/awscliv2.zip -d /tmp && /tmp/aws/install && rm -rf /tmp/aws /tmp/awscliv2.zip

echo "==> Installing ec2-instance-connect"
dnf install -y ec2-instance-connect

echo "==> Installing fish shell"
dnf install -y fish
chsh -s /usr/bin/fish ec2-user

echo "==> Installing Go"
dnf install -y golang

echo "==> Installing Rust"
sudo -u ec2-user sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path'

echo "==> Installing Zed remote server"
ARCH=$(uname -m)
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  | gunzip > /usr/local/bin/zed-remote
chmod +x /usr/local/bin/zed-remote
echo "Installed Zed remote server ${RELEASE} (${ARCH})"

echo "==> Installing Headless Chrome"
cat > /etc/yum.repos.d/google-chrome.repo <<'REPO'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
REPO
dnf install -y google-chrome-stable

echo "==> All done"
`

// Run sends the dev toolchain install script to the instance via SSM send-command.
// Returns the SSM Command ID.
func Run(ctx context.Context, profile, region, instanceID string) (commandID string, err error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
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
