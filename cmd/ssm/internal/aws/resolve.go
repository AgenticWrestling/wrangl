// cmd/ssm/internal/aws/resolve.go
package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	cfg, err := LoadConfig(ctx, profile, region)
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
	return listInstancesByState(ctx, profile, region, "running")
}

func ListStoppedInstances(ctx context.Context, profile, region string) ([]Instance, error) {
	return listInstancesByState(ctx, profile, region, "stopped", "stopping")
}

func listInstancesByState(ctx context.Context, profile, region string, states ...string) ([]Instance, error) {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return nil, err
	}

	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-state-name"), Values: states},
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

func StartInstance(ctx context.Context, profile, region, instanceID string) error {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return err
	}
	if _, err := client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return err
	}
	waiter := ec2.NewInstanceRunningWaiter(client)
	return waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, 5*time.Minute)
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
