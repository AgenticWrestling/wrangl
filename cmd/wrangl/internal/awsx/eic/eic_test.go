package eic

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
)

type pusher struct {
	in      *ec2instanceconnect.SendSSHPublicKeyInput
	success bool
}

func (p *pusher) SendSSHPublicKey(_ context.Context, in *ec2instanceconnect.SendSSHPublicKeyInput, _ ...func(*ec2instanceconnect.Options)) (*ec2instanceconnect.SendSSHPublicKeyOutput, error) {
	p.in = in
	return &ec2instanceconnect.SendSSHPublicKeyOutput{Success: p.success}, nil
}

func TestPushPublicKey(t *testing.T) {
	p := &pusher{success: true}
	if err := PushPublicKey(context.Background(), p, "i-1", "ec2-user", []byte("ssh-ed25519 AAA")); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(p.in.InstanceOSUser) != "ec2-user" || aws.ToString(p.in.SSHPublicKey) != "ssh-ed25519 AAA" {
		t.Fatalf("%+v", p.in)
	}
	p.success = false
	if err := PushPublicKey(context.Background(), p, "i-1", "u", []byte("k")); err == nil {
		t.Fatal("want error when EIC reports Success=false")
	}
}

type fakeEP struct {
	eps        []types.Ec2InstanceConnectEndpoint
	created    int
	cidr       string
	perms      []types.IpPermission
	authorized []*ec2.AuthorizeSecurityGroupIngressInput
	revoked    []*ec2.RevokeSecurityGroupIngressInput
}

func (f *fakeEP) DescribeInstanceConnectEndpoints(_ context.Context, in *ec2.DescribeInstanceConnectEndpointsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceConnectEndpointsOutput, error) {
	return &ec2.DescribeInstanceConnectEndpointsOutput{InstanceConnectEndpoints: f.eps}, nil
}
func (f *fakeEP) CreateInstanceConnectEndpoint(context.Context, *ec2.CreateInstanceConnectEndpointInput, ...func(*ec2.Options)) (*ec2.CreateInstanceConnectEndpointOutput, error) {
	f.created++
	f.eps = []types.Ec2InstanceConnectEndpoint{{InstanceConnectEndpointId: aws.String("eice-1"), State: types.Ec2InstanceConnectEndpointStateCreateComplete}}
	return &ec2.CreateInstanceConnectEndpointOutput{InstanceConnectEndpoint: &f.eps[0]}, nil
}
func (f *fakeEP) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{CidrBlock: aws.String(f.cidr)}}}, nil
}
func (f *fakeEP) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{{IpPermissions: f.perms}}}, nil
}
func (f *fakeEP) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.authorized = append(f.authorized, in)
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}
func (f *fakeEP) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.revoked = append(f.revoked, in)
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func TestEnsureEndpoint(t *testing.T) {
	PollInterval = time.Millisecond
	ctx := context.Background()
	f := &fakeEP{}
	if err := EnsureEndpoint(ctx, f, "vpc-1", "subnet-1", nil); err != nil || f.created != 1 {
		t.Fatal(err, f.created)
	}
	if err := EnsureEndpoint(ctx, f, "vpc-1", "subnet-1", nil); err != nil || f.created != 1 {
		t.Fatalf("an available endpoint must be reused: %v created=%d", err, f.created)
	}
}

func TestEnsureSSHFromVPC(t *testing.T) {
	ctx := context.Background()
	f := &fakeEP{cidr: "10.0.0.0/16"}
	if err := EnsureSSHFromVPC(ctx, f, "vpc-1", []string{"sg-1"}); err != nil || len(f.authorized) != 1 {
		t.Fatal(err, len(f.authorized))
	}
	f.perms = []types.IpPermission{{FromPort: aws.Int32(22), ToPort: aws.Int32(22), IpRanges: []types.IpRange{{CidrIp: aws.String("10.0.0.0/16")}}}}
	if err := EnsureSSHFromVPC(ctx, f, "vpc-1", []string{"sg-1"}); err != nil || len(f.authorized) != 1 {
		t.Fatalf("existing rule must not be added again: %v %d", err, len(f.authorized))
	}
}

func TestLegacyRules(t *testing.T) {
	ctx := context.Background()
	f := &fakeEP{perms: []types.IpPermission{
		{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("1.2.3.4/32"), Description: aws.String("ssm-managed")}}},
		{IpProtocol: aws.String("tcp"), IpRanges: []types.IpRange{{CidrIp: aws.String("10.0.0.0/8"), Description: aws.String("other")}}},
	}}
	found, err := FindLegacyRules(ctx, f, []string{"sg-1"})
	if err != nil || len(found["sg-1"]) != 1 {
		t.Fatal(found, err)
	}
	if err := RevokeLegacyRules(ctx, f, found); err != nil || len(f.revoked) != 1 {
		t.Fatal(err)
	}
}
