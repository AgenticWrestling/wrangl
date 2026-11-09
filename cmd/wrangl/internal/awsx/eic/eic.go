// Package eic wraps EC2 Instance Connect: pushing a one-time SSH public key (valid 60 s) and
// the EIC Endpoint plus security-group rule that let `open-tunnel` reach a private instance.
package eic

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
)

// KeyPusher is the EIC client subset used here.
type KeyPusher interface {
	SendSSHPublicKey(context.Context, *ec2instanceconnect.SendSSHPublicKeyInput, ...func(*ec2instanceconnect.Options)) (*ec2instanceconnect.SendSSHPublicKeyOutput, error)
}

// PushPublicKey registers pubKey for osUser on the instance; it is accepted for 60 seconds.
func PushPublicKey(ctx context.Context, c KeyPusher, instanceID, osUser string, pubKey []byte) error {
	out, err := c.SendSSHPublicKey(ctx, &ec2instanceconnect.SendSSHPublicKeyInput{
		InstanceId: aws.String(instanceID), InstanceOSUser: aws.String(osUser), SSHPublicKey: aws.String(string(pubKey)),
	})
	if err != nil {
		return err
	}
	if out != nil && !out.Success {
		return fmt.Errorf("EC2 Instance Connect refused the key for %s@%s", osUser, instanceID)
	}
	return nil
}

// Endpoints is the EC2 subset used to manage the EIC endpoint and security-group rule.
type Endpoints interface {
	DescribeInstanceConnectEndpoints(context.Context, *ec2.DescribeInstanceConnectEndpointsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceConnectEndpointsOutput, error)
	CreateInstanceConnectEndpoint(context.Context, *ec2.CreateInstanceConnectEndpointInput, ...func(*ec2.Options)) (*ec2.CreateInstanceConnectEndpointOutput, error)
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupIngress(context.Context, *ec2.RevokeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
}

// PollInterval is how often EnsureEndpoint polls a creating endpoint; tests shorten it.
var PollInterval = 5 * time.Second

// EnsureEndpoint makes sure the VPC has a usable EIC endpoint, creating one in subnetID if not,
// and waits (up to 5 minutes) until it is available. Progress lines go to log (may be nil).
func EnsureEndpoint(ctx context.Context, c Endpoints, vpcID, subnetID string, log io.Writer) error {
	out, err := c.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
		Filters: []types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return fmt.Errorf("describing EIC endpoints: %w", err)
	}
	var id string
	for _, ep := range out.InstanceConnectEndpoints {
		switch ep.State {
		case types.Ec2InstanceConnectEndpointStateCreateComplete:
			return nil
		case types.Ec2InstanceConnectEndpointStateCreateInProgress:
			id = aws.ToString(ep.InstanceConnectEndpointId)
			if log != nil {
				fmt.Fprintf(log, "EIC endpoint %s is being created, waiting...\n", id)
			}
		}
	}
	if id == "" {
		if log != nil {
			fmt.Fprintf(log, "creating an EIC endpoint in %s (about a minute)...\n", subnetID)
		}
		cr, err := c.CreateInstanceConnectEndpoint(ctx, &ec2.CreateInstanceConnectEndpointInput{SubnetId: aws.String(subnetID)})
		if err != nil {
			return fmt.Errorf("creating EIC endpoint: %w", err)
		}
		id = aws.ToString(cr.InstanceConnectEndpoint.InstanceConnectEndpointId)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := c.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{InstanceConnectEndpointIds: []string{id}})
		if err != nil {
			return err
		}
		if len(d.InstanceConnectEndpoints) > 0 {
			switch d.InstanceConnectEndpoints[0].State {
			case types.Ec2InstanceConnectEndpointStateCreateComplete:
				return nil
			case types.Ec2InstanceConnectEndpointStateCreateFailed:
				return fmt.Errorf("EIC endpoint creation failed")
			}
		}
		select {
		case <-time.After(PollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for the EIC endpoint to become available")
}

// EnsureSSHFromVPC makes each security group allow TCP 22 from the VPC's CIDR (the endpoint's
// network interface lives inside the VPC).
func EnsureSSHFromVPC(ctx context.Context, c Endpoints, vpcID string, sgIDs []string) error {
	v, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return fmt.Errorf("describing VPC %s: %w", vpcID, err)
	}
	if len(v.Vpcs) == 0 {
		return fmt.Errorf("VPC %s not found", vpcID)
	}
	cidr := aws.ToString(v.Vpcs[0].CidrBlock)
	for _, sg := range sgIDs {
		if err := ensureSSHRule(ctx, c, sg, cidr); err != nil {
			return fmt.Errorf("SG %s: %w", sg, err)
		}
	}
	return nil
}

func ensureSSHRule(ctx context.Context, c Endpoints, sgID, cidr string) error {
	out, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{sgID}})
	if err != nil {
		return err
	}
	if len(out.SecurityGroups) == 0 {
		return fmt.Errorf("security group %s not found", sgID)
	}
	for _, p := range out.SecurityGroups[0].IpPermissions {
		if aws.ToInt32(p.FromPort) == 22 && aws.ToInt32(p.ToPort) == 22 {
			for _, r := range p.IpRanges {
				if aws.ToString(r.CidrIp) == cidr {
					return nil
				}
			}
		}
	}
	_, err = c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
			IpRanges: []types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String("SSH via EIC (wrangl dev)")}},
		}},
	})
	return err
}

// LegacyRuleDescription marks the all-ports-from-my-IP rules the old ssm tool created.
const LegacyRuleDescription = "ssm-managed"

// FindLegacyRules returns, per security group, the permissions carrying the old ssm-managed
// marker. wrangl never creates these; `dev setup` only reports and optionally revokes them.
func FindLegacyRules(ctx context.Context, c Endpoints, sgIDs []string) (map[string][]types.IpPermission, error) {
	found := map[string][]types.IpPermission{}
	for _, sg := range sgIDs {
		out, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{sg}})
		if err != nil {
			return nil, err
		}
		for _, g := range out.SecurityGroups {
			for _, p := range g.IpPermissions {
				var ranges []types.IpRange
				for _, r := range p.IpRanges {
					if aws.ToString(r.Description) == LegacyRuleDescription {
						ranges = append(ranges, r)
					}
				}
				if len(ranges) > 0 {
					q := p
					q.IpRanges = ranges
					found[sg] = append(found[sg], q)
				}
			}
		}
	}
	return found, nil
}

// RevokeLegacyRules removes what FindLegacyRules returned.
func RevokeLegacyRules(ctx context.Context, c Endpoints, rules map[string][]types.IpPermission) error {
	for sg, perms := range rules {
		if _, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(sg), IpPermissions: perms}); err != nil {
			return fmt.Errorf("SG %s: %w", sg, err)
		}
	}
	return nil
}
