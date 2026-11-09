package awsx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Instance is the slice of an EC2 instance the dev commands need.
type Instance struct {
	ID               string
	Name             string
	Region           string
	State            string
	VPCID            string
	SubnetID         string
	PrivateIP        string
	SecurityGroupIDs []string
}

// EC2API is the subset of the EC2 client awsx uses; *ec2.Client satisfies it and tests use Fake.
type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}

// Session is an AWS account + region pair with its EC2 client.
type Session struct {
	Profile, Region string
	EC2             EC2API
	// Wait blocks until the instance is running; replaced in tests.
	Wait func(ctx context.Context, id string) error
}

// NewSession loads the config for profile/region and builds the clients.
func NewSession(ctx context.Context, profile, region string) (*Session, error) {
	cfg, err := LoadConfig(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	c := ec2.NewFromConfig(cfg)
	return &Session{Profile: profile, Region: cfg.Region, EC2: c, Wait: func(ctx context.Context, id string) error {
		return ec2.NewInstanceRunningWaiter(c).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 5*time.Minute)
	}}, nil
}

// ParseRegionName splits "region/name".
func ParseRegionName(arg string) (region, name string, err error) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected region/name, got %q", arg)
	}
	return parts[0], parts[1], nil
}

// LooksLikeRegion reports whether s has the shape of an AWS region (us-west-2).
func LooksLikeRegion(s string) bool {
	parts := strings.SplitN(s, "-", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return false
	}
	last := parts[2]
	if i := strings.LastIndex(last, "-"); i >= 0 {
		last = last[i+1:]
	}
	if last == "" {
		return false
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// IsInstanceID reports whether s looks like an EC2 instance id.
func IsInstanceID(s string) bool {
	if !strings.HasPrefix(s, "i-") || len(s) < 10 {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func fromSDK(i types.Instance, region string) Instance {
	inst := Instance{
		ID: aws.ToString(i.InstanceId), Region: region,
		VPCID: aws.ToString(i.VpcId), SubnetID: aws.ToString(i.SubnetId), PrivateIP: aws.ToString(i.PrivateIpAddress),
	}
	if i.State != nil {
		inst.State = string(i.State.Name)
	}
	for _, sg := range i.SecurityGroups {
		inst.SecurityGroupIDs = append(inst.SecurityGroupIDs, aws.ToString(sg.GroupId))
	}
	for _, t := range i.Tags {
		if aws.ToString(t.Key) == "Name" {
			inst.Name = aws.ToString(t.Value)
		}
	}
	return inst
}

func (s *Session) describe(ctx context.Context, in *ec2.DescribeInstancesInput) ([]Instance, error) {
	var out []Instance
	for {
		r, err := s.EC2.DescribeInstances(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, res := range r.Reservations {
			for _, i := range res.Instances {
				out = append(out, fromSDK(i, s.Region))
			}
		}
		if r.NextToken == nil {
			return out, nil
		}
		in.NextToken = r.NextToken
	}
}

func stateFilter(states []string) []types.Filter {
	if len(states) == 0 {
		return nil
	}
	return []types.Filter{{Name: aws.String("instance-state-name"), Values: states}}
}

// List returns instances in the given states (none: every state except terminated).
func (s *Session) List(ctx context.Context, states ...string) ([]Instance, error) {
	if len(states) == 0 {
		states = []string{"pending", "running", "stopping", "stopped"}
	}
	return s.describe(ctx, &ec2.DescribeInstancesInput{Filters: stateFilter(states)})
}

// Get returns one instance by id.
func (s *Session) Get(ctx context.Context, id string) (*Instance, error) {
	l, err := s.describe(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, fmt.Errorf("instance %s not found in %s", id, s.Region)
	}
	return &l[0], nil
}

// ByName finds the one instance whose Name tag is name in the given states (default running).
func (s *Session) ByName(ctx context.Context, name string, states ...string) (*Instance, error) {
	if len(states) == 0 {
		states = []string{"running"}
	}
	f := append(stateFilter(states), types.Filter{Name: aws.String("tag:Name"), Values: []string{name}})
	l, err := s.describe(ctx, &ec2.DescribeInstancesInput{Filters: f})
	if err != nil {
		return nil, err
	}
	switch len(l) {
	case 0:
		return nil, fmt.Errorf("no %s instance with Name=%q found in %s", strings.Join(states, "/"), name, s.Region)
	case 1:
		return &l[0], nil
	}
	ids := make([]string, len(l))
	for i, m := range l {
		ids[i] = m.ID
	}
	return nil, fmt.Errorf("multiple instances with Name=%q in %s: %s", name, s.Region, strings.Join(ids, ", "))
}

// Start starts a stopped instance and waits until it is running.
func (s *Session) Start(ctx context.Context, id string) error {
	if _, err := s.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
		return err
	}
	if s.Wait == nil {
		return nil
	}
	return s.Wait(ctx, id)
}

// Stop stops a running instance (it does not wait).
func (s *Session) Stop(ctx context.Context, id string) error {
	_, err := s.EC2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}})
	return err
}
