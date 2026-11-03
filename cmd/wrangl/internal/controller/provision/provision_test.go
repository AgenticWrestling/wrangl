package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

func spec() Spec {
	return Spec{Runner: "r1", Template: "go", Arch: "arm64", MinVCPU: 4, MinRAMMiB: 16384, DiskGiB: 100,
		AMI:      "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64",
		SubnetID: "subnet-1", SecurityGroupID: "sg-1", InstanceProfile: "wrangl-runner", UserData: "#!/bin/bash\n"}
}

func TestLaunchTagsEveryResource(t *testing.T) {
	f := NewFake()
	p := &Provisioner{EC2: f, SSM: f, ControllerID: "ctl-1"}
	inst, err := p.Launch(context.Background(), spec())
	if err != nil {
		t.Fatal(err)
	}
	in := f.Runs[0]
	want := Tags("ctl-1", "r1", "go")
	kinds := map[types.ResourceType]bool{}
	for _, ts := range in.TagSpecifications {
		kinds[ts.ResourceType] = true
		got := map[string]string{}
		for _, tg := range ts.Tags {
			got[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s tag %s = %q, want %q", ts.ResourceType, k, got[k], v)
			}
		}
	}
	for _, k := range []types.ResourceType{types.ResourceTypeInstance, types.ResourceTypeVolume, types.ResourceTypeNetworkInterface} {
		if !kinds[k] {
			t.Errorf("no tags for %s", k)
		}
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
		t.Error("shutdown behaviour must be terminate")
	}
	if in.MetadataOptions.HttpTokens != types.HttpTokensStateRequired {
		t.Error("IMDSv2 not required")
	}
	bd := in.BlockDeviceMappings[0]
	if aws.ToString(bd.DeviceName) != "/dev/xvda" || aws.ToInt32(bd.Ebs.VolumeSize) != 100 || !aws.ToBool(bd.Ebs.DeleteOnTermination) {
		t.Errorf("root volume: %+v", bd)
	}
	if aws.ToString(in.ImageId) != "ami-0fake0000000000001" {
		t.Errorf("AMI parameter not resolved: %s", aws.ToString(in.ImageId))
	}
	if inst.Type != "m7g.xlarge" || inst.PrivateIP == "" {
		t.Errorf("instance %+v", inst)
	}
}

func TestChooseInstanceType(t *testing.T) {
	f := NewFake()
	p := &Provisioner{EC2: f, SSM: f, ControllerID: "c"}
	ctx := context.Background()
	s := spec()
	if got, _ := p.ChooseInstanceType(ctx, s); got != "m7g.xlarge" {
		t.Fatalf("auto arm64 4/16: %s", got)
	}
	s.MinVCPU, s.MinRAMMiB = 2, 16384
	if got, _ := p.ChooseInstanceType(ctx, s); got != "r7g.large" {
		t.Fatalf("auto arm64 2/16: %s", got)
	}
	s.Arch = "x86_64"
	if got, _ := p.ChooseInstanceType(ctx, s); got != "m7i.xlarge" {
		t.Fatalf("auto x86: %s", got)
	}
	s = spec()
	s.InstanceTypes = []string{"m7g.large", "m7g.2xlarge", "m7g.xlarge"} // first too small
	if got, _ := p.ChooseInstanceType(ctx, s); got != "m7g.2xlarge" {
		t.Fatalf("allowlist preference: %s", got)
	}
	s.MinVCPU = 64
	if _, err := p.ChooseInstanceType(ctx, s); err == nil {
		t.Fatal("impossible spec accepted")
	}
}

func TestTerminateRefusesUnownedInstances(t *testing.T) {
	f := NewFake()
	p := &Provisioner{EC2: f, SSM: f, ControllerID: "ctl-1"}
	ctx := context.Background()
	f.AddForeign("i-untagged", map[string]string{"Name": "prod-db"})
	f.AddForeign("i-other", Tags("ctl-2", "x", "go"))
	for _, id := range []string{"i-untagged", "i-other"} {
		if err := p.Terminate(ctx, id); !errors.Is(err, ErrNotOwned) {
			t.Fatalf("%s: want ErrNotOwned, got %v", id, err)
		}
	}
	if len(f.Terminated) != 0 {
		t.Fatalf("terminated %v", f.Terminated)
	}
	inst, _ := p.Launch(ctx, spec())
	if err := p.Terminate(ctx, inst.ID); err != nil || f.State(inst.ID) != "terminated" {
		t.Fatalf("own instance: %v %s", err, f.State(inst.ID))
	}
	if err := p.Terminate(ctx, inst.ID); err != nil {
		t.Fatalf("already terminated should be a no-op: %v", err)
	}
	if err := p.Terminate(ctx, "i-gone"); err != nil {
		t.Fatalf("missing instance should be a no-op: %v", err)
	}
}

func TestTeardownScopes(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	a := &Provisioner{EC2: f, SSM: f, ControllerID: "ctl-a"}
	b := &Provisioner{EC2: f, SSM: f, ControllerID: "ctl-b"}
	a1, _ := a.Launch(ctx, spec())
	b1, _ := b.Launch(ctx, spec())
	f.AddForeign("i-untagged", map[string]string{"Name": "prod-db"})

	done, err := a.Teardown(ctx, "ctl-a")
	if err != nil || len(done) != 1 || done[0].ID != a1.ID || f.State(b1.ID) == "terminated" {
		t.Fatalf("scoped teardown: %v %v", done, err)
	}
	done, err = a.Teardown(ctx, "") // --all-controllers
	if err != nil || len(done) != 1 || done[0].ID != b1.ID {
		t.Fatalf("all-controllers teardown: %v %v", done, err)
	}
	if f.State("i-untagged") != "running" {
		t.Fatal("teardown touched an untagged instance")
	}
	if left, _ := a.ListManaged(ctx, ""); len(left) != 0 {
		t.Fatalf("still listed: %v", left)
	}
}

func TestRenderUserData(t *testing.T) {
	sum := strings.Repeat("a", 64)
	ud, err := RenderUserData(UserDataParams{EnrollURL: "http://10.0.1.5:4223", Token: "wre_abc-123", SHA256: map[string]string{"aarch64": sum}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aarch64) SUM=" + sum, "/dl/wrangl-linux-${ARCH}", "sha256sum -c", "command -v git", "--token \"wre_abc-123\""} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data missing %q:\n%s", want, ud)
		}
	}
	for _, bad := range []UserDataParams{
		{EnrollURL: "http://x;rm -rf /", Token: "t", SHA256: map[string]string{"aarch64": sum}},
		{EnrollURL: "http://10.0.1.5:4223", Token: "t\"; reboot", SHA256: map[string]string{"aarch64": sum}},
		{EnrollURL: "http://10.0.1.5:4223", Token: "t", SHA256: map[string]string{"aarch64": "nothex"}},
	} {
		if _, err := RenderUserData(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

type blockedSSM struct{}

func (blockedSSM) GetParameter(ctx context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestResolveAMITimesOut(t *testing.T) {
	old := ssmTimeout
	ssmTimeout = 50 * time.Millisecond
	defer func() { ssmTimeout = old }()
	p := &Provisioner{SSM: blockedSSM{}}
	start := time.Now()
	_, err := p.resolveAMI(context.Background(), "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64")
	if err == nil || !strings.Contains(err.Error(), "pin an AMI id") {
		t.Fatalf("want a hint to pin the AMI, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not time out")
	}
	// A pinned id never touches SSM.
	if got, err := p.resolveAMI(context.Background(), "ami-123"); err != nil || got != "ami-123" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestSortCandidatesPrefersNoLocalStorage(t *testing.T) {
	mk := func(name string, vcpu int32, mem int64, local bool) types.InstanceTypeInfo {
		return types.InstanceTypeInfo{InstanceType: types.InstanceType(name), InstanceStorageSupported: aws.Bool(local),
			VCpuInfo: &types.VCpuInfo{DefaultVCpus: aws.Int32(vcpu)}, MemoryInfo: &types.MemoryInfo{SizeInMiB: aws.Int64(mem)}}
	}
	c := []types.InstanceTypeInfo{mk("im4gn.large", 2, 8192, true), mk("m7g.large", 2, 8192, false), mk("m7g.xlarge", 4, 16384, false), mk("c7g.large", 2, 4096, false)}
	sortCandidates(c)
	var got []string
	for _, x := range c {
		got = append(got, string(x.InstanceType))
	}
	want := []string{"c7g.large", "m7g.large", "im4gn.large", "m7g.xlarge"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestMarketplaceSearchAndLaunchCheck(t *testing.T) {
	f := NewFake()
	img := func(id, name string, a types.ArchitectureValues) types.Image {
		return types.Image{ImageId: aws.String(id), Name: aws.String(name), Architecture: a}
	}
	f.Marketplace = []types.Image{
		img("ami-a", "genymotion-ami-12.0-v14.2-arm64", types.ArchitectureValuesArm64),
		img("ami-b", "genymotion-ami-15.0-v14.2-arm64", types.ArchitectureValuesArm64),
		img("ami-c", "genymotion-ami-10.0-v14.1-x86_64", types.ArchitectureValuesX8664),
		img("ami-d", "ubuntu-pro", types.ArchitectureValuesArm64),
	}
	f.Unsubscribed = map[string]string{"ami-b": "https://aws.amazon.com/marketplace/pp?sku=abc123"}
	p := &Provisioner{EC2: f, SSM: f, ControllerID: "c"}
	ctx := context.Background()

	if _, err := p.SearchMarketplace(ctx, "ge", ""); err == nil {
		t.Fatal("two letters must be refused")
	}
	arm, err := p.SearchMarketplace(ctx, "genymotion", "arm64")
	if err != nil || len(arm) != 2 || arm[0].Name != "genymotion-ami-15.0-v14.2-arm64" {
		t.Fatalf("arm64 search (newest names first): %+v %v", arm, err)
	}
	if all, _ := p.SearchMarketplace(ctx, "genymotion", ""); len(all) != 3 {
		t.Fatalf("both architectures: %+v", all)
	}

	if c := p.CheckLaunch(ctx, "ami-a", "m7g.large", "subnet-1", "sg-1"); !c.OK {
		t.Fatalf("a subscribed image would launch: %+v", c)
	}
	c := p.CheckLaunch(ctx, "ami-b", "m7g.large", "subnet-1", "sg-1")
	if c.OK || !c.OptInRequired || c.SubscribeURL != "https://aws.amazon.com/marketplace/pp?sku=abc123" {
		t.Fatalf("an unsubscribed image must say where to subscribe: %+v", c)
	}
	if len(f.Runs) != 0 {
		t.Fatal("a check must never launch anything")
	}
}
