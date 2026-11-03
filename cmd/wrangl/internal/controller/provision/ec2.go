package provision

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// EC2API is the subset of the EC2 client used here (faked in tests).
type EC2API interface {
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
}

// SSMAPI resolves public AMI parameters such as
// /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64.
type SSMAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Spec is what to launch.
type Spec struct {
	Runner, Template                           string
	Arch                                       string // arm64 | x86_64 (EC2 naming)
	MinVCPU, MinRAMMiB                         int
	DiskGiB                                    int
	InstanceTypes                              []string // preference allowlist; empty = choose automatically
	AMI                                        string   // ami-… or an SSM parameter path
	SubnetID, SecurityGroupID, InstanceProfile string
	UserData                                   string
}

// Instance is a launched or discovered instance.
type Instance struct {
	ID, Type, AZ, PrivateIP, State string
	Tags                           map[string]string
}

// Provisioner talks to EC2 on behalf of one controller.
type Provisioner struct {
	EC2          EC2API
	SSM          SSMAPI
	ControllerID string
}

// ErrNotOwned is returned when asked to terminate an instance this controller did not tag.
var ErrNotOwned = errors.New("instance is not managed by this controller")

// Launch starts one runner instance with every resource tagged.
func (p *Provisioner) Launch(ctx context.Context, s Spec) (*Instance, error) {
	if p.ControllerID == "" {
		return nil, errors.New("controller id is empty: run `wrangl controller init`")
	}
	ami, err := p.resolveAMI(ctx, s.AMI)
	if err != nil {
		return nil, err
	}
	rootDev, err := p.rootDevice(ctx, ami)
	if err != nil {
		return nil, err
	}
	itype, err := p.ChooseInstanceType(ctx, s)
	if err != nil {
		return nil, err
	}
	tags := Tags(p.ControllerID, s.Runner, s.Template)
	var ec2Tags []types.Tag
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ec2Tags = append(ec2Tags, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	in := &ec2.RunInstancesInput{
		ImageId:      aws.String(ami),
		InstanceType: types.InstanceType(itype),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		SubnetId:     aws.String(s.SubnetID),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(s.UserData))),
		// The runner powers itself off when orphaned; this makes that a terminate.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String(rootDev),
			Ebs: &types.EbsBlockDevice{
				VolumeSize:          aws.Int32(int32(s.DiskGiB)),
				VolumeType:          types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true),
				Encrypted:           aws.Bool(true),
			},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: ec2Tags},
			{ResourceType: types.ResourceTypeVolume, Tags: ec2Tags},
			{ResourceType: types.ResourceTypeNetworkInterface, Tags: ec2Tags},
		},
	}
	if s.SecurityGroupID != "" {
		in.SecurityGroupIds = []string{s.SecurityGroupID}
	}
	if s.InstanceProfile != "" {
		in.IamInstanceProfile = &types.IamInstanceProfileSpecification{Name: aws.String(s.InstanceProfile)}
	}
	out, err := p.EC2.RunInstances(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("RunInstances: %w", err)
	}
	if len(out.Instances) != 1 {
		return nil, fmt.Errorf("RunInstances returned %d instances", len(out.Instances))
	}
	return fromEC2(out.Instances[0]), nil
}

// ssmTimeout bounds the SSM parameter lookup for a default AMI.
var ssmTimeout = 15 * time.Second

func (p *Provisioner) resolveAMI(ctx context.Context, ami string) (string, error) {
	if strings.HasPrefix(ami, "ami-") {
		return ami, nil
	}
	if !strings.HasPrefix(ami, "/") {
		return "", fmt.Errorf("ami %q: want ami-… or an SSM parameter path", ami)
	}
	// An unreachable SSM endpoint would otherwise hang the launch for minutes.
	ctx, cancel := context.WithTimeout(ctx, ssmTimeout)
	defer cancel()
	out, err := p.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(ami)})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("resolve AMI parameter %s: no answer from SSM within %s; pin an AMI id in the template (ami: ami-…) or check the SSM endpoint", ami, ssmTimeout)
		}
		return "", fmt.Errorf("resolve AMI parameter %s: %w", ami, err)
	}
	return aws.ToString(out.Parameter.Value), nil
}

func (p *Provisioner) rootDevice(ctx context.Context, ami string) (string, error) {
	out, err := p.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
	if err != nil {
		return "", fmt.Errorf("DescribeImages %s: %w", ami, err)
	}
	if len(out.Images) != 1 || out.Images[0].RootDeviceName == nil {
		return "", fmt.Errorf("AMI %s not found or has no root device", ami)
	}
	return *out.Images[0].RootDeviceName, nil
}

// ChooseInstanceType returns the first allowlisted type meeting the minimums, or (with no
// allowlist) the smallest current-generation type of the right arch that meets them.
func (p *Provisioner) ChooseInstanceType(ctx context.Context, s Spec) (string, error) {
	meets := func(it types.InstanceTypeInfo) bool {
		vcpu := int(aws.ToInt32(it.VCpuInfo.DefaultVCpus))
		mem := int(aws.ToInt64(it.MemoryInfo.SizeInMiB))
		archOK := false
		for _, a := range it.ProcessorInfo.SupportedArchitectures {
			archOK = archOK || string(a) == s.Arch
		}
		return archOK && vcpu >= s.MinVCPU && mem >= s.MinRAMMiB
	}
	if len(s.InstanceTypes) > 0 {
		types_ := make([]types.InstanceType, len(s.InstanceTypes))
		for i, t := range s.InstanceTypes {
			types_[i] = types.InstanceType(t)
		}
		out, err := p.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: types_})
		if err != nil {
			return "", fmt.Errorf("DescribeInstanceTypes: %w", err)
		}
		byName := map[string]types.InstanceTypeInfo{}
		for _, it := range out.InstanceTypes {
			byName[string(it.InstanceType)] = it
		}
		for _, t := range s.InstanceTypes { // preference order
			if it, ok := byName[t]; ok && meets(it) {
				return t, nil
			}
		}
		return "", fmt.Errorf("none of %v meets %s, %d vCPU, %d MiB", s.InstanceTypes, s.Arch, s.MinVCPU, s.MinRAMMiB)
	}
	var cands []types.InstanceTypeInfo
	in := &ec2.DescribeInstanceTypesInput{Filters: []types.Filter{
		{Name: aws.String("processor-info.supported-architecture"), Values: []string{s.Arch}},
		{Name: aws.String("current-generation"), Values: []string{"true"}},
		{Name: aws.String("bare-metal"), Values: []string{"false"}},
	}}
	for {
		out, err := p.EC2.DescribeInstanceTypes(ctx, in)
		if err != nil {
			return "", fmt.Errorf("DescribeInstanceTypes: %w", err)
		}
		for _, it := range out.InstanceTypes {
			if meets(it) {
				cands = append(cands, it)
			}
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no current-generation %s type has %d vCPU and %d MiB", s.Arch, s.MinVCPU, s.MinRAMMiB)
	}
	sortCandidates(cands)
	return string(cands[0].InstanceType), nil
}

// sortCandidates orders instance types smallest first: fewest vCPUs, then least memory. On a
// tie, types without local instance storage come first (storage-optimised families such as
// im4gn cost about three times as much as general purpose ones for the same shape), then the
// name, so the choice is deterministic.
func sortCandidates(cands []types.InstanceTypeInfo) {
	local := func(t types.InstanceTypeInfo) bool { return aws.ToBool(t.InstanceStorageSupported) }
	sort.Slice(cands, func(i, j int) bool {
		vi, vj := aws.ToInt32(cands[i].VCpuInfo.DefaultVCpus), aws.ToInt32(cands[j].VCpuInfo.DefaultVCpus)
		if vi != vj {
			return vi < vj
		}
		mi, mj := aws.ToInt64(cands[i].MemoryInfo.SizeInMiB), aws.ToInt64(cands[j].MemoryInfo.SizeInMiB)
		if mi != mj {
			return mi < mj
		}
		if li, lj := local(cands[i]), local(cands[j]); li != lj {
			return !li
		}
		return cands[i].InstanceType < cands[j].InstanceType
	})
}

// Describe returns one instance (nil if it no longer exists).
func (p *Provisioner) Describe(ctx context.Context, id string) (*Instance, error) {
	out, err := p.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("DescribeInstances %s: %w", id, err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			return fromEC2(i), nil
		}
	}
	return nil, nil
}

// Terminate terminates id only after confirming, on the live instance, that it carries
// this controller's tags. An already-gone instance is not an error.
func (p *Provisioner) Terminate(ctx context.Context, id string) error {
	inst, err := p.Describe(ctx, id)
	if err != nil {
		return err
	}
	if inst == nil || inst.State == "terminated" {
		return nil
	}
	if !Owned(inst.Tags, p.ControllerID) {
		return fmt.Errorf("%s: %w (tags %v)", id, ErrNotOwned, inst.Tags)
	}
	_, err = p.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return fmt.Errorf("TerminateInstances %s: %w", id, err)
	}
	return nil
}

// ListManaged returns live (not terminated) instances tagged wrangl:managed=true, limited
// to controllerID unless it is empty.
func (p *Provisioner) ListManaged(ctx context.Context, controllerID string) ([]*Instance, error) {
	filters := []types.Filter{
		{Name: aws.String("tag:" + TagManaged), Values: []string{"true"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}
	if controllerID != "" {
		filters = append(filters, types.Filter{Name: aws.String("tag:" + TagController), Values: []string{controllerID}})
	}
	var res []*Instance
	in := &ec2.DescribeInstancesInput{Filters: filters}
	for {
		out, err := p.EC2.DescribeInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("DescribeInstances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				if inst := fromEC2(i); Owned(inst.Tags, controllerID) {
					res = append(res, inst)
				}
			}
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Tags[TagRunner] < res[j].Tags[TagRunner] })
	return res, nil
}

// Teardown terminates every instance ListManaged returns, re-checking ownership on each
// live instance. An empty controllerID means every wrangl-managed instance in the region
// (teardown --all-controllers). It returns the instances it terminated.
func (p *Provisioner) Teardown(ctx context.Context, controllerID string) ([]*Instance, error) {
	insts, err := p.ListManaged(ctx, controllerID)
	if err != nil {
		return nil, err
	}
	scoped := &Provisioner{EC2: p.EC2, SSM: p.SSM, ControllerID: controllerID}
	var done []*Instance
	var errs []error
	for _, i := range insts {
		if err := scoped.Terminate(ctx, i.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, i)
	}
	return done, errors.Join(errs...)
}

func fromEC2(i types.Instance) *Instance {
	inst := &Instance{
		ID:        aws.ToString(i.InstanceId),
		Type:      string(i.InstanceType),
		PrivateIP: aws.ToString(i.PrivateIpAddress),
		Tags:      map[string]string{},
	}
	if i.Placement != nil {
		inst.AZ = aws.ToString(i.Placement.AvailabilityZone)
	}
	if i.State != nil {
		inst.State = string(i.State.Name)
	}
	for _, t := range i.Tags {
		inst.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return inst
}
