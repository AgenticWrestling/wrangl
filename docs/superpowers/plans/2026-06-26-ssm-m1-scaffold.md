# SSM Tool — M1: Go Module Scaffold & Shared Infrastructure

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Initialise the Go module and implement all shared infrastructure: profile listing, instance resolution, interactive picker, and terminal spinner.

**Architecture:** Single Go module at `~/code/devhost`. All shared code lives under `cmd/ssm/internal/`. The cobra root command is wired but all subcommands are stubs returning `nil` until later milestones.

**Tech Stack:** Go 1.21+, AWS SDK v2 (EC2), github.com/spf13/cobra, github.com/ktr0731/go-fuzzyfinder, ini parsing via github.com/go-ini/ini

## Global Constraints

- Module path: `github.com/agenticwrestling/wrangl`
- Binary output: `~/bin/ssm` via `go build -o ~/bin/ssm ./cmd/ssm`
- CGO_ENABLED=0 (statically linked)
- Target OS: darwin (local), linux (remote provisioning scripts only)
- Login user on instances: `ec2-user`
- Default region fallback: `us-east-1`

---

### Task 1: Initialise Go module and directory structure

**Files:**
- Create: `go.mod`
- Create: `go.sum` (generated)
- Create: `cmd/ssm/main.go`
- Create: `cmd/ssm/internal/aws/profiles.go`
- Create: `cmd/ssm/internal/aws/resolve.go`
- Create: `cmd/ssm/internal/ui/picker.go`
- Create: `cmd/ssm/internal/ui/spinner.go`

**Interfaces:**
- Produces: nothing yet — scaffold only

- [ ] **Step 1: Create directory structure**

```bash
cd ~/code/devhost
mkdir -p cmd/ssm/internal/aws
mkdir -p cmd/ssm/internal/ui
mkdir -p cmd/ssm/internal/ssh
```

- [ ] **Step 2: Initialise Go module**

```bash
cd ~/code/devhost
go mod init github.com/agenticwrestling/wrangl
```

Expected: `go.mod` created with `module github.com/agenticwrestling/wrangl` and current Go version.

- [ ] **Step 3: Add dependencies**

```bash
cd ~/code/devhost
go get github.com/spf13/cobra@latest
go get github.com/aws/aws-sdk-go-v2@latest
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/service/ec2@latest
go get github.com/ktr0731/go-fuzzyfinder@latest
go get gopkg.in/ini.v1@latest
```

- [ ] **Step 4: Write stub main.go**

```go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "ssm",
		Short: "Manage EC2 dev instances via SSM",
	}

	root.AddCommand(connectCmd(), tunnelCmd(), setupCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func connectCmd() *cobra.Command {
	return &cobra.Command{Use: "connect [region/name]", Short: "Open SSM terminal session", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func tunnelCmd() *cobra.Command {
	return &cobra.Command{Use: "tunnel [region/name] <rport>[:<lport>]", Short: "SSM port-forward tunnel", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [region/name]", Short: "Provision dev toolchain on instance", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}
```

- [ ] **Step 5: Verify it builds**

```bash
cd ~/code/devhost
go build ./cmd/ssm
```

Expected: no errors, `ssm` binary in current directory.

- [ ] **Step 6: Commit**

```bash
cd ~/code/devhost
git init
git add go.mod go.sum cmd/
git commit -m "feat: initialise ssm Go module with stub subcommands"
```

---

### Task 2: Profile listing

**Files:**
- Create: `cmd/ssm/internal/aws/profiles.go`
- Create: `cmd/ssm/internal/aws/profiles_test.go`

**Interfaces:**
- Produces: `ListProfiles() ([]string, error)` — returns profile names from `~/.aws/config`

- [ ] **Step 1: Write the failing test**

```go
// cmd/ssm/internal/aws/profiles_test.go
package aws_test

import (
	"os"
	"path/filepath"
	"testing"

	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
)

func TestListProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte("[default]\n[profile dev]\n[profile staging]\n"), 0600)
	t.Setenv("AWS_CONFIG_FILE", cfg)

	profiles, err := ssmaws.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 3 {
		t.Fatalf("want 3 profiles, got %d: %v", len(profiles), profiles)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/aws/...
```

Expected: compile error — `ListProfiles` not defined.

- [ ] **Step 3: Implement profiles.go**

```go
// cmd/ssm/internal/aws/profiles.go
package aws

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/ini.v1"
)

func ListProfiles() ([]string, error) {
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".aws", "config")
	}

	cfg, err := ini.Load(path)
	if err != nil {
		return nil, err
	}

	var profiles []string
	for _, section := range cfg.Sections() {
		name := section.Name()
		if name == "DEFAULT" {
			continue
		}
		if name == "default" {
			profiles = append(profiles, "default")
			continue
		}
		if strings.HasPrefix(name, "profile ") {
			profiles = append(profiles, strings.TrimPrefix(name, "profile "))
		}
	}
	return profiles, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/aws/... -run TestListProfiles -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/aws/profiles.go cmd/ssm/internal/aws/profiles_test.go
git commit -m "feat: implement ListProfiles from ~/.aws/config"
```

---

### Task 3: Instance resolver

**Files:**
- Create: `cmd/ssm/internal/aws/resolve.go`
- Create: `cmd/ssm/internal/aws/resolve_test.go`

**Interfaces:**
- Consumes: AWS SDK v2 EC2 client
- Produces:
  ```go
  type Instance struct {
      ID     string
      Name   string
      Region string
      VPCID  string
      SubnetID string
      PrivateIP string
      SecurityGroupIDs []string
  }

  // ResolveInstance finds a running instance by Name tag in the given region.
  // Returns error if 0 or 2+ matches found.
  func ResolveInstance(ctx context.Context, profile, region, name string) (*Instance, error)

  // ListInstances returns all running instances in the given region.
  func ListInstances(ctx context.Context, profile, region string) ([]Instance, error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
// cmd/ssm/internal/aws/resolve_test.go
package aws_test

import (
	"context"
	"testing"

	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
)

// Integration guard — skip if no AWS credentials available
func TestResolveInstance_noMatch(t *testing.T) {
	t.Skip("integration test — run manually with valid AWS credentials")
	_, err := ssmaws.ResolveInstance(context.Background(), "default", "us-east-1", "nonexistent-host-xyz")
	if err == nil {
		t.Fatal("expected error for no match")
	}
}

func TestParseRegionName(t *testing.T) {
	region, name, err := ssmaws.ParseRegionName("us-west-2/myhost")
	if err != nil || region != "us-west-2" || name != "myhost" {
		t.Fatalf("got region=%q name=%q err=%v", region, name, err)
	}

	_, _, err = ssmaws.ParseRegionName("myhost")
	if err == nil {
		t.Fatal("expected error for missing region")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/aws/... -run TestParseRegionName -v
```

Expected: compile error — `ParseRegionName` not defined.

- [ ] **Step 3: Implement resolve.go**

```go
// cmd/ssm/internal/aws/resolve.go
package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type Instance struct {
	ID               string
	Name             string
	Region           string
	VPCID            string
	SubnetID         string
	PrivateIP        string
	SecurityGroupIDs []string
}

func ParseRegionName(arg string) (region, name string, err error) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected region/name, got %q", arg)
	}
	return parts[0], parts[1], nil
}

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

func ResolveInstance(ctx context.Context, profile, region, name string) (*Instance, error) {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return nil, err
	}

	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{name}},
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	})
	if err != nil {
		return nil, err
	}

	var matches []Instance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			matches = append(matches, instanceFromSDK(i, region))
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no running instance with Name=%q found in %s", name, region)
	case 1:
		return &matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return nil, fmt.Errorf("multiple instances with Name=%q in %s: %s", name, region, strings.Join(ids, ", "))
	}
}

func ListInstances(ctx context.Context, profile, region string) ([]Instance, error) {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return nil, err
	}

	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	})
	if err != nil {
		return nil, err
	}

	var instances []Instance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			instances = append(instances, instanceFromSDK(i, region))
		}
	}
	return instances, nil
}

func instanceFromSDK(i types.Instance, region string) Instance {
	inst := Instance{
		ID:        aws.ToString(i.InstanceId),
		Region:    region,
		VPCID:     aws.ToString(i.VpcId),
		SubnetID:  aws.ToString(i.SubnetId),
		PrivateIP: aws.ToString(i.PrivateIpAddress),
	}
	for _, sg := range i.SecurityGroups {
		inst.SecurityGroupIDs = append(inst.SecurityGroupIDs, aws.ToString(sg.GroupId))
	}
	for _, tag := range i.Tags {
		if aws.ToString(tag.Key) == "Name" {
			inst.Name = aws.ToString(tag.Value)
		}
	}
	return inst
}
```

- [ ] **Step 4: Run tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/aws/... -run TestParseRegionName -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/aws/resolve.go cmd/ssm/internal/aws/resolve_test.go
git commit -m "feat: implement instance resolver and ParseRegionName"
```

---

### Task 4: Interactive picker and spinner

**Files:**
- Create: `cmd/ssm/internal/ui/picker.go`
- Create: `cmd/ssm/internal/ui/spinner.go`

**Interfaces:**
- Produces:
  ```go
  // PickProfile prompts the user to pick an AWS profile.
  // Returns the selected profile name.
  func PickProfile(profiles []string) (string, error)

  // PickInstance prompts the user to pick from a list of instances.
  func PickInstance(instances []aws.Instance) (*aws.Instance, error)

  // Spinner runs f() while showing a status message, then stops.
  func Spinner(msg string, f func() error) error
  ```

- [ ] **Step 1: Implement picker.go**

No unit test possible for interactive UI — build verification is the test.

```go
// cmd/ssm/internal/ui/picker.go
package ui

import (
	"fmt"

	"github.com/ktr0731/go-fuzzyfinder"
	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
)

func PickProfile(profiles []string) (string, error) {
	if len(profiles) == 0 {
		return "", fmt.Errorf("no AWS profiles found in ~/.aws/config")
	}
	idx, err := fuzzyfinder.Find(profiles, func(i int) string { return profiles[i] })
	if err != nil {
		return "", err
	}
	return profiles[idx], nil
}

func PickInstance(instances []ssmaws.Instance) (*ssmaws.Instance, error) {
	if len(instances) == 0 {
		return nil, fmt.Errorf("no running instances found")
	}
	idx, err := fuzzyfinder.Find(instances, func(i int) string {
		inst := instances[i]
		return fmt.Sprintf("%-30s  %-20s  %s", inst.Name, inst.ID, inst.PrivateIP)
	})
	if err != nil {
		return nil, err
	}
	return &instances[idx], nil
}
```

- [ ] **Step 2: Implement spinner.go**

```go
// cmd/ssm/internal/ui/spinner.go
package ui

import (
	"fmt"
	"os"
	"time"
)

func Spinner(msg string, f func() error) error {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan error, 1)
	go func() { done <- f() }()

	i := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return err
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "\r%s %s", frames[i%len(frames)], msg)
			i++
		}
	}
}
```

- [ ] **Step 3: Verify it builds**

```bash
cd ~/code/devhost
go build ./cmd/ssm/...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/ui/
git commit -m "feat: implement interactive picker and terminal spinner"
```

---

### Task 5: Shared instance resolution helper used by all subcommands

**Files:**
- Modify: `cmd/ssm/main.go`

**Interfaces:**
- Produces: `resolveTarget(args []string, profile string) (*aws.Instance, error)` — used by all three subcommands

- [ ] **Step 1: Add resolveTarget to main.go**

Replace `main.go` content with:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/ui"
)

func main() {
	root := &cobra.Command{
		Use:   "ssm",
		Short: "Manage EC2 dev instances via SSM",
	}
	root.AddCommand(connectCmd(), tunnelCmd(), setupCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolveTarget picks a profile interactively, then resolves region/name arg
// to a running instance. If args is empty or no region/name provided, lists
// all running instances for interactive selection.
func resolveTarget(args []string, defaultRegion string) (string, *ssmaws.Instance, error) {
	profiles, err := ssmaws.ListProfiles()
	if err != nil {
		return "", nil, fmt.Errorf("listing profiles: %w", err)
	}
	profile, err := ui.PickProfile(profiles)
	if err != nil {
		return "", nil, err
	}

	ctx := context.Background()

	if len(args) > 0 {
		region, name, err := ssmaws.ParseRegionName(args[0])
		if err != nil {
			return "", nil, err
		}
		inst, err := ssmaws.ResolveInstance(ctx, profile, region, name)
		return profile, inst, err
	}

	if defaultRegion == "" {
		defaultRegion = "us-east-1"
	}
	instances, err := ssmaws.ListInstances(ctx, profile, defaultRegion)
	if err != nil {
		return "", nil, err
	}
	inst, err := ui.PickInstance(instances)
	return profile, inst, err
}

func connectCmd() *cobra.Command {
	return &cobra.Command{Use: "connect [region/name]", Short: "Open SSM terminal session", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func tunnelCmd() *cobra.Command {
	return &cobra.Command{Use: "tunnel [region/name] <rport>[:<lport>]", Short: "SSM port-forward tunnel", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [region/name]", Short: "Provision dev toolchain on instance", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}
```

- [ ] **Step 2: Verify it builds**

```bash
cd ~/code/devhost
go build ./cmd/ssm/...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/main.go
git commit -m "feat: add resolveTarget shared helper for all subcommands"
```
