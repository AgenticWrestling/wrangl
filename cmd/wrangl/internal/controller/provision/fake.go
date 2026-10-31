package provision

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Fake is an in-memory EC2 + SSM for tests. It records every RunInstances input and
// enforces nothing beyond what the provisioner relies on.
type Fake struct {
	mu         sync.Mutex
	next       int
	Instances  map[string]*types.Instance
	Runs       []*ec2.RunInstancesInput
	Terminated []string
	Types      []types.InstanceTypeInfo
	IPPrefix   string // private IPs are IPPrefix + n; default "10.0.1."
	FixedIP    string // if set, every instance gets this private IP (e2e tests on loopback)
}

// NewFake returns a fake with a small instance-type catalogue.
func NewFake() *Fake {
	it := func(name string, vcpu int32, mib int64, archs ...types.ArchitectureType) types.InstanceTypeInfo {
		return types.InstanceTypeInfo{
			InstanceType:  types.InstanceType(name),
			VCpuInfo:      &types.VCpuInfo{DefaultVCpus: aws.Int32(vcpu)},
			MemoryInfo:    &types.MemoryInfo{SizeInMiB: aws.Int64(mib)},
			ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: archs},
		}
	}
	return &Fake{
		Instances: map[string]*types.Instance{},
		Types: []types.InstanceTypeInfo{
			it("m7g.large", 2, 8192, types.ArchitectureTypeArm64),
			it("m7g.xlarge", 4, 16384, types.ArchitectureTypeArm64),
			it("r7g.large", 2, 16384, types.ArchitectureTypeArm64),
			it("m7g.2xlarge", 8, 32768, types.ArchitectureTypeArm64),
			it("m7i.xlarge", 4, 16384, types.ArchitectureTypeX8664),
		},
		IPPrefix: "10.0.1.",
	}
}

// AddForeign inserts an instance not launched by the provisioner (for ownership tests).
func (f *Fake) AddForeign(id string, tags map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ts []types.Tag
	for k, v := range tags {
		ts = append(ts, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	f.Instances[id] = &types.Instance{InstanceId: aws.String(id), Tags: ts,
		State: &types.InstanceState{Name: types.InstanceStateNameRunning}}
}

func (f *Fake) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("i-%017x", f.next)
	var tags []types.Tag
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == types.ResourceTypeInstance {
			tags = ts.Tags
		}
	}
	ip := fmt.Sprintf("%s%d", f.IPPrefix, 10+f.next)
	if f.FixedIP != "" {
		ip = f.FixedIP
	}
	inst := &types.Instance{
		InstanceId:       aws.String(id),
		InstanceType:     in.InstanceType,
		PrivateIpAddress: aws.String(ip),
		Placement:        &types.Placement{AvailabilityZone: aws.String("us-west-2a")},
		State:            &types.InstanceState{Name: types.InstanceStateNamePending},
		Tags:             tags,
	}
	f.Instances[id] = inst
	f.Runs = append(f.Runs, in)
	return &ec2.RunInstancesOutput{Instances: []types.Instance{*inst}}, nil
}

func (f *Fake) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		inst, ok := f.Instances[id]
		if !ok {
			return nil, errors.New("InvalidInstanceID.NotFound: " + id)
		}
		inst.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
		f.Terminated = append(f.Terminated, id)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *Fake) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.Instance
	if len(in.InstanceIds) > 0 {
		for _, id := range in.InstanceIds {
			inst, ok := f.Instances[id]
			if !ok {
				return nil, errors.New("InvalidInstanceID.NotFound: " + id)
			}
			out = append(out, *inst)
		}
	} else {
		for _, inst := range f.Instances {
			if matches(inst, in.Filters) {
				out = append(out, *inst)
			}
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: out}}}, nil
}

func matches(inst *types.Instance, filters []types.Filter) bool {
	tags := fromEC2(*inst).Tags
	for _, f := range filters {
		name := aws.ToString(f.Name)
		var got string
		switch {
		case name == "instance-state-name":
			got = string(inst.State.Name)
		case len(name) > 4 && name[:4] == "tag:":
			got = tags[name[4:]]
		default:
			continue
		}
		ok := false
		for _, v := range f.Values {
			ok = ok || v == got
		}
		if !ok {
			return false
		}
	}
	return true
}

func (f *Fake) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	if len(in.InstanceTypes) == 0 {
		var arch string
		for _, flt := range in.Filters {
			if aws.ToString(flt.Name) == "processor-info.supported-architecture" {
				arch = flt.Values[0]
			}
		}
		var out []types.InstanceTypeInfo
		for _, it := range f.Types {
			for _, a := range it.ProcessorInfo.SupportedArchitectures {
				if string(a) == arch {
					out = append(out, it)
				}
			}
		}
		return &ec2.DescribeInstanceTypesOutput{InstanceTypes: out}, nil
	}
	var out []types.InstanceTypeInfo
	for _, want := range in.InstanceTypes {
		for _, it := range f.Types {
			if it.InstanceType == want {
				out = append(out, it)
			}
		}
	}
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: out}, nil
}

func (f *Fake) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String(in.ImageIds[0]), RootDeviceName: aws.String("/dev/xvda")}}}, nil
}

func (f *Fake) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name, Value: aws.String("ami-0fake0000000000001")}}, nil
}

// State returns an instance's current state name ("" if unknown).
func (f *Fake) State(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inst, ok := f.Instances[id]; ok {
		return string(inst.State.Name)
	}
	return ""
}

// SetRunning moves a pending instance to running.
func (f *Fake) SetRunning(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Instances[id].State = &types.InstanceState{Name: types.InstanceStateNameRunning}
}
