# SSM Tool — M4: `ssm setup` Phase 1 — EIC Endpoint, VPC/SG, SSH Config

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the local setup phase of `ssm setup`: ensure an EC2 Instance Connect Endpoint exists in the instance's VPC, ensure port 22 is open within the VPC, generate a local Ed25519 keypair if needed, push the public key via EIC, and write the `~/.ssh/config` Host block.

**Architecture:** Three focused internal packages — `eic` (endpoint + key push), `vpc` (security group), `ssh` (config + keygen). The `ssm setup` subcommand calls them in sequence with spinner feedback. M5 adds the remote provisioning phase on top.

**Tech Stack:** AWS SDK v2 (EC2, EC2InstanceConnect), Go stdlib `crypto/ed25519`, `golang.org/x/crypto/ssh`, M1 shared infrastructure

## Global Constraints

- Module path: `github.com/nreid/devhost`
- CGO_ENABLED=0
- M1 must be complete before starting M4
- Login user on instances: `ec2-user`

---

### Task 1: SSH keypair generation and config helpers

**Files:**
- Create: `cmd/ssm/internal/ssh/keygen.go`
- Create: `cmd/ssm/internal/ssh/keygen_test.go`
- Create: `cmd/ssm/internal/ssh/config.go`
- Create: `cmd/ssm/internal/ssh/config_test.go`

**Interfaces:**
- Produces:
  ```go
  // EnsureKeyPair creates ~/.ssh/id_ed25519 if absent. Returns public key bytes (authorized_keys format).
  func EnsureKeyPair() (pubKeyBytes []byte, err error)

  // UpsertHostBlock writes or replaces a Host block in ~/.ssh/config.
  func UpsertHostBlock(name, instanceID, region, profile string) error
  ```

- [ ] **Step 1: Add golang.org/x/crypto dependency**

```bash
cd ~/code/devhost
go get golang.org/x/crypto@latest
```

- [ ] **Step 2: Write failing tests for keygen**

```go
// cmd/ssm/internal/ssh/keygen_test.go
package ssh_test

import (
	"os"
	"path/filepath"
	"testing"

	sshpkg "github.com/nreid/devhost/cmd/ssm/internal/ssh"
)

func TestEnsureKeyPair_creates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pub, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(pub) == 0 {
		t.Fatal("expected non-empty public key")
	}
	if _, err := os.Stat(filepath.Join(dir, ".ssh", "id_ed25519")); err != nil {
		t.Fatalf("private key not created: %v", err)
	}
}

func TestEnsureKeyPair_idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pub1, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pub2, err := sshpkg.EnsureKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if string(pub1) != string(pub2) {
		t.Fatal("second call returned different key — not idempotent")
	}
}
```

- [ ] **Step 3: Run to verify they fail**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/ssh/... -v
```

Expected: compile error — package does not exist.

- [ ] **Step 4: Create directory and implement keygen.go**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/ssh
```

```go
// cmd/ssm/internal/ssh/keygen.go
package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

func EnsureKeyPair() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(home, ".ssh")
	privPath := filepath.Join(sshDir, "id_ed25519")
	pubPath := privPath + ".pub"

	if _, err := os.Stat(privPath); err == nil {
		pub, err := os.ReadFile(pubPath)
		return pub, err
	}

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	privPEM, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privPEM), 0600); err != nil {
		return nil, err
	}

	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	pubBytes := gossh.MarshalAuthorizedKey(sshPub)
	if err := os.WriteFile(pubPath, pubBytes, 0644); err != nil {
		return nil, err
	}
	return pubBytes, nil
}
```

- [ ] **Step 5: Write failing test for UpsertHostBlock**

```go
// cmd/ssm/internal/ssh/config_test.go
package ssh_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sshpkg "github.com/nreid/devhost/cmd/ssm/internal/ssh"
)

func TestUpsertHostBlock_creates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.MkdirAll(filepath.Join(dir, ".ssh"), 0700)

	err := sshpkg.UpsertHostBlock("myhost", "i-0abc123", "us-west-2", "dev")
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".ssh", "config"))
	s := string(data)
	if !strings.Contains(s, "Host myhost") {
		t.Errorf("expected Host block, got:\n%s", s)
	}
	if !strings.Contains(s, "i-0abc123") {
		t.Errorf("expected instance ID in config, got:\n%s", s)
	}
}

func TestUpsertHostBlock_updates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	sshDir := filepath.Join(dir, ".ssh")
	os.MkdirAll(sshDir, 0700)

	// First write with old instance ID
	sshpkg.UpsertHostBlock("myhost", "i-OLD", "us-west-2", "dev")
	// Overwrite with new instance ID
	sshpkg.UpsertHostBlock("myhost", "i-NEW", "us-west-2", "dev")

	data, _ := os.ReadFile(filepath.Join(dir, ".ssh", "config"))
	s := string(data)
	if strings.Contains(s, "i-OLD") {
		t.Errorf("old instance ID should have been replaced:\n%s", s)
	}
	if !strings.Contains(s, "i-NEW") {
		t.Errorf("new instance ID not found:\n%s", s)
	}
}
```

- [ ] **Step 6: Implement config.go**

```go
// cmd/ssm/internal/ssh/config.go
package ssh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func UpsertHostBlock(name, instanceID, region, profile string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")

	block := fmt.Sprintf(`Host %s
  HostName %s
  User ec2-user
  IdentityFile ~/.ssh/id_ed25519
  ProxyCommand aws ec2-instance-connect open-tunnel --instance-id %%h --region %s --profile %s
`, name, instanceID, region, profile)

	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if err == nil {
		out := removeHostBlock(string(existing), name)
		return os.WriteFile(configPath, []byte(out+block), 0600)
	}
	return os.WriteFile(configPath, []byte(block), 0600)
}

// removeHostBlock removes an existing Host block for the given name from config content.
func removeHostBlock(content, name string) string {
	var out []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Host ") {
			inBlock = strings.TrimPrefix(trimmed, "Host ") == name
		}
		if !inBlock {
			out = append(out, line)
		}
	}
	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") && len(result) > 0 {
		result += "\n"
	}
	return result
}
```

- [ ] **Step 7: Run all ssh package tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/ssh/... -v
```

Expected: all PASS

- [ ] **Step 8: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/ssh/
git commit -m "feat: SSH keypair generation and ~/.ssh/config host block management"
```

---

### Task 2: EIC Endpoint management and public key push

**Files:**
- Create: `cmd/ssm/internal/eic/eic.go`
- Create: `cmd/ssm/internal/eic/eic_test.go`

**Interfaces:**
- Produces:
  ```go
  // EnsureEndpoint checks for an existing EICE in the VPC; creates one if absent.
  // Blocks until the endpoint is available (spinner handled by caller).
  func EnsureEndpoint(ctx context.Context, profile, region, vpcID, subnetID string) error

  // PushPublicKey registers the local Ed25519 public key with the instance via EIC (valid 60s).
  func PushPublicKey(ctx context.Context, profile, region, instanceID string, pubKey []byte) error
  ```

- [ ] **Step 1: Add EC2 Instance Connect SDK dependency**

```bash
cd ~/code/devhost
go get github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect@latest
```

- [ ] **Step 2: Write failing tests**

```go
// cmd/ssm/internal/eic/eic_test.go
package eic_test

import (
	"testing"
)

// Integration tests — run manually with valid AWS credentials and a real VPC.
func TestEnsureEndpoint_integration(t *testing.T) {
	t.Skip("integration test")
}

func TestPushPublicKey_integration(t *testing.T) {
	t.Skip("integration test")
}
```

- [ ] **Step 3: Create directory and implement eic.go**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/eic
```

```go
// cmd/ssm/internal/eic/eic.go
package eic

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
)

func newEC2Client(ctx context.Context, profile, region string) (*ec2.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}

func EnsureEndpoint(ctx context.Context, profile, region, vpcID, subnetID string) error {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return err
	}

	out, err := client.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("state"), Values: []string{"create-complete"}},
		},
	})
	if err != nil {
		return fmt.Errorf("describing EIC endpoints: %w", err)
	}
	if len(out.InstanceConnectEndpoints) > 0 {
		return nil // already exists
	}

	create, err := client.CreateInstanceConnectEndpoint(ctx, &ec2.CreateInstanceConnectEndpointInput{
		SubnetId: aws.String(subnetID),
	})
	if err != nil {
		return fmt.Errorf("creating EIC endpoint: %w", err)
	}
	endpointID := aws.ToString(create.InstanceConnectEndpoint.InstanceConnectEndpointId)

	// Poll until available (up to 5 minutes).
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		desc, err := client.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			InstanceConnectEndpointIds: []string{endpointID},
		})
		if err != nil {
			return err
		}
		if len(desc.InstanceConnectEndpoints) > 0 {
			state := desc.InstanceConnectEndpoints[0].State
			if state == types.InstanceConnectEndpointStateCreateComplete {
				return nil
			}
			if state == types.InstanceConnectEndpointStateCreateFailed {
				return fmt.Errorf("EIC endpoint creation failed")
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timed out waiting for EIC endpoint to become available")
}

func PushPublicKey(ctx context.Context, profile, region, instanceID string, pubKey []byte) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return err
	}
	client := ec2instanceconnect.NewFromConfig(cfg)

	_, err = client.SendSSHPublicKey(ctx, &ec2instanceconnect.SendSSHPublicKeyInput{
		InstanceId:     aws.String(instanceID),
		InstanceOSUser: aws.String("ec2-user"),
		SSHPublicKey:   aws.String(string(pubKey)),
	})
	return err
}
```

- [ ] **Step 4: Build to verify**

```bash
cd ~/code/devhost
go build ./cmd/ssm/...
```

Expected: no errors.

- [ ] **Step 5: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/eic/
git commit -m "feat: EIC endpoint management and public key push"
```

---

### Task 3: VPC security group helper

**Files:**
- Create: `cmd/ssm/internal/vpc/vpc.go`

**Interfaces:**
- Produces:
  ```go
  // EnsureSSHIngressFromVPC adds an inbound rule for TCP 22 from the VPC CIDR
  // to each security group if not already present.
  func EnsureSSHIngressFromVPC(ctx context.Context, profile, region, vpcID string, sgIDs []string) error
  ```

- [ ] **Step 1: Create directory and implement**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/vpc
```

```go
// cmd/ssm/internal/vpc/vpc.go
package vpc

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func EnsureSSHIngressFromVPC(ctx context.Context, profile, region, vpcID string, sgIDs []string) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)

	// Get VPC CIDR.
	vpcOut, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil || len(vpcOut.Vpcs) == 0 {
		return fmt.Errorf("describing VPC %s: %w", vpcID, err)
	}
	cidr := aws.ToString(vpcOut.Vpcs[0].CidrBlock)

	for _, sgID := range sgIDs {
		if err := ensureSSHRule(ctx, client, sgID, cidr); err != nil {
			return fmt.Errorf("SG %s: %w", sgID, err)
		}
	}
	return nil
}

func ensureSSHRule(ctx context.Context, client *ec2.Client, sgID, cidr string) error {
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil || len(out.SecurityGroups) == 0 {
		return err
	}

	for _, perm := range out.SecurityGroups[0].IpPermissions {
		if aws.ToInt32(perm.FromPort) == 22 && aws.ToInt32(perm.ToPort) == 22 {
			for _, r := range perm.IpRanges {
				if aws.ToString(r.CidrIp) == cidr {
					return nil // rule already exists
				}
			}
		}
	}

	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges:   []types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String("SSH via EIC")}},
			},
		},
	})
	return err
}
```

- [ ] **Step 2: Build to verify**

```bash
cd ~/code/devhost
go build ./cmd/ssm/...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/vpc/
git commit -m "feat: VPC security group SSH ingress helper"
```

---

### Task 4: Wire Phase 1 into `ssm setup` subcommand

**Files:**
- Modify: `cmd/ssm/main.go` — replace `setupCmd()` stub with Phase 1 logic (Phase 2 added in M5)

- [ ] **Step 1: Replace setupCmd stub in main.go**

Add imports:
```go
"github.com/nreid/devhost/cmd/ssm/internal/eic"
"github.com/nreid/devhost/cmd/ssm/internal/vpc"
sshpkg "github.com/nreid/devhost/cmd/ssm/internal/ssh"
```

Replace `setupCmd()`:

```go
func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup [region/name]",
		Short: "Provision dev toolchain on instance",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, inst, err := resolveTarget(args, "")
			if err != nil {
				return err
			}
			ctx := context.Background()

			// Phase 1a: EIC Endpoint
			if err := ui.Spinner("Ensuring EIC endpoint in VPC...", func() error {
				return eic.EnsureEndpoint(ctx, profile, inst.Region, inst.VPCID, inst.SubnetID)
			}); err != nil {
				return fmt.Errorf("EIC endpoint: %w", err)
			}
			fmt.Println("✓ EIC endpoint ready")

			// Phase 1b: Security group
			if err := ui.Spinner("Ensuring SSH ingress rule...", func() error {
				return vpc.EnsureSSHIngressFromVPC(ctx, profile, inst.Region, inst.VPCID, inst.SecurityGroupIDs)
			}); err != nil {
				return fmt.Errorf("security group: %w", err)
			}
			fmt.Println("✓ Security group rule present")

			// Phase 1c: SSH keypair
			pubKey, err := sshpkg.EnsureKeyPair()
			if err != nil {
				return fmt.Errorf("SSH keypair: %w", err)
			}
			fmt.Println("✓ SSH keypair ready")

			// Phase 1d: Push public key via EIC
			if err := eic.PushPublicKey(ctx, profile, inst.Region, inst.ID, pubKey); err != nil {
				return fmt.Errorf("push SSH public key: %w", err)
			}
			fmt.Println("✓ SSH public key registered (valid 60s)")

			// Phase 1e: SSH config
			if err := sshpkg.UpsertHostBlock(inst.Name, inst.ID, inst.Region, profile); err != nil {
				return fmt.Errorf("SSH config: %w", err)
			}
			fmt.Printf("✓ ~/.ssh/config updated — connect with: ssh %s\n", inst.Name)

			// Phase 2 (provisioning) added in M5.
			return nil
		},
	}
}
```

- [ ] **Step 2: Build**

```bash
cd ~/code/devhost
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
~/bin/ssm setup --help
```

Expected: help text shown, no errors.

- [ ] **Step 3: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/main.go
git commit -m "feat: ssm setup Phase 1 — EIC, VPC, SSH keypair, SSH config"
```
