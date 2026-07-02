package vpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

const myIPRuleDescription = "ssm-managed"

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

// EnsureMyIPIngress opens all TCP ports on the given security groups from the
// caller's current public IP. Any previously added ssm-managed rule is revoked
// first, so stale rules from old IPs don't accumulate.
func EnsureMyIPIngress(ctx context.Context, profile, region string, sgIDs []string) (string, error) {
	ip, err := myPublicIP()
	if err != nil {
		return "", fmt.Errorf("detecting public IP: %w", err)
	}
	cidr := ip + "/32"

	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return "", err
	}
	client := ec2.NewFromConfig(cfg)

	for _, sgID := range sgIDs {
		if err := upsertMyIPRule(ctx, client, sgID, cidr); err != nil {
			return "", fmt.Errorf("SG %s: %w", sgID, err)
		}
	}
	return ip, nil
}

func myPublicIP() (string, error) {
	resp, err := http.Get("https://checkip.amazonaws.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func upsertMyIPRule(ctx context.Context, client *ec2.Client, sgID, cidr string) error {
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil {
		return err
	}
	if len(out.SecurityGroups) == 0 {
		return fmt.Errorf("security group %s not found", sgID)
	}

	// Revoke any existing ssm-managed rule regardless of IP.
	var toRevoke []types.IpPermission
	for _, perm := range out.SecurityGroups[0].IpPermissions {
		var stale []types.IpRange
		for _, r := range perm.IpRanges {
			if aws.ToString(r.Description) == myIPRuleDescription {
				if aws.ToString(r.CidrIp) == cidr {
					// Already correct — nothing to do for this SG.
					return nil
				}
				stale = append(stale, r)
			}
		}
		if len(stale) > 0 {
			p := perm
			p.IpRanges = stale
			toRevoke = append(toRevoke, p)
		}
	}
	if len(toRevoke) > 0 {
		if _, err := client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: toRevoke,
		}); err != nil {
			return fmt.Errorf("revoking stale rule: %w", err)
		}
	}

	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("-1"), // all traffic
				IpRanges:   []types.IpRange{{CidrIp: aws.String(cidr), Description: aws.String(myIPRuleDescription)}},
			},
		},
	})
	return err
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
