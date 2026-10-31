package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
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
	for _, want := range []string{"aarch64) SUM=" + sum, "/dl/wrangl-linux-${ARCH}", "sha256sum -c", "--token \"wre_abc-123\""} {
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
