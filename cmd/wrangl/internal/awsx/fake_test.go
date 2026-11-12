package awsx

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEC2 serves a fixed set of instances and records start/stop calls.
type fakeEC2 struct {
	insts            []types.Instance
	started, stopped []string
}

func inst(id, name, state string) types.Instance {
	return types.Instance{
		InstanceId: aws.String(id), VpcId: aws.String("vpc-1"), SubnetId: aws.String("subnet-1"),
		PrivateIpAddress: aws.String("10.0.0.5"), State: &types.InstanceState{Name: types.InstanceStateName(state)},
		SecurityGroups: []types.GroupIdentifier{{GroupId: aws.String("sg-1")}},
		Tags:           []types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
	}
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	var out []types.Instance
	for _, i := range f.insts {
		ok := true
		for _, fl := range in.Filters {
			switch aws.ToString(fl.Name) {
			case "instance-state-name":
				ok = ok && contains(fl.Values, string(i.State.Name))
			case "tag:Name":
				ok = ok && contains(fl.Values, aws.ToString(i.Tags[0].Value))
			}
		}
		if len(in.InstanceIds) > 0 {
			ok = ok && contains(in.InstanceIds, aws.ToString(i.InstanceId))
		}
		if ok {
			out = append(out, i)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: out}}}, nil
}

func (f *fakeEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.started = append(f.started, in.InstanceIds...)
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.stopped = append(f.stopped, in.InstanceIds...)
	return &ec2.StopInstancesOutput{}, nil
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
