package vpc

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

func EnsureSSHIngressFromVPC(ctx context.Context, profile, region, vpcID string, sgIDs []string) error {
	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return err
	}
	client := ec2.NewFromConfig(cfg)

	// Get VPC CIDR.
	vpcOut, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil || len(vpcOut.Vpcs) == 0 {
		return fmt.Errorf("describing VPC %s: %w", vpcID, err)
	}
	cidr := aws.ToString(vpcOut.Vpcs[0].CidrBlock)

	for _, sgID := range sgIDs {
		if err := ensureSSHRule(ctx, client, sgID, cidr); err != nil {
			return fmt.Errorf("SG %s: %w", sgID, err)
		}
	}
	return nil
}

func ensureSSHRule(ctx context.Context, client *ec2.Client, sgID, cidr string) error {
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil {
		return err
	}
	if len(out.SecurityGroups) == 0 {
		return fmt.Errorf("security group %s not found", sgID)
	}

	for _, perm := range out.SecurityGroups[0].IpPermissions {
		if aws.ToInt32(perm.FromPort) == 22 && aws.ToInt32(perm.ToPort) == 22 {
			for _, r := range perm.IpRanges {
				if aws.ToString(r.CidrIp) == cidr {
					return nil // rule already exists
				}
			}
		}
	}

	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges:   []types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String("SSH via EIC")}},
			},
		},
	})
	return err
}
