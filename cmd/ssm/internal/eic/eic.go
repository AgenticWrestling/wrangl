// cmd/ssm/internal/eic/eic.go
package eic

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

func newEC2Client(ctx context.Context, profile, region string) (*ec2.Client, error) {
	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(cfg), nil
}

// EnsureEndpoint checks for an existing EICE in the VPC; creates one if absent.
// Blocks until the endpoint is available (spinner handled by caller).
func EnsureEndpoint(ctx context.Context, profile, region, vpcID, subnetID string) error {
	client, err := newEC2Client(ctx, profile, region)
	if err != nil {
		return err
	}

	out, err := client.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
		Filters: []types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("state"), Values: []string{"create-complete"}},
		},
	})
	if err != nil {
		return fmt.Errorf("describing EIC endpoints: %w", err)
	}
	if len(out.InstanceConnectEndpoints) > 0 {
		return nil // already exists
	}

	create, err := client.CreateInstanceConnectEndpoint(ctx, &ec2.CreateInstanceConnectEndpointInput{
		SubnetId: aws.String(subnetID),
	})
	if err != nil {
		return fmt.Errorf("creating EIC endpoint: %w", err)
	}
	endpointID := aws.ToString(create.InstanceConnectEndpoint.InstanceConnectEndpointId)

	// Poll until available (up to 5 minutes).
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		desc, err := client.DescribeInstanceConnectEndpoints(ctx, &ec2.DescribeInstanceConnectEndpointsInput{
			InstanceConnectEndpointIds: []string{endpointID},
		})
		if err != nil {
			return err
		}
		if len(desc.InstanceConnectEndpoints) > 0 {
			state := desc.InstanceConnectEndpoints[0].State
			if state == types.Ec2InstanceConnectEndpointStateCreateComplete {
				return nil
			}
			if state == types.Ec2InstanceConnectEndpointStateCreateFailed {
				return fmt.Errorf("EIC endpoint creation failed")
			}
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for EIC endpoint to become available")
}

// PushPublicKey registers the local Ed25519 public key with the instance via EIC (valid 60s).
func PushPublicKey(ctx context.Context, profile, region, instanceID string, pubKey []byte) error {
	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return err
	}
	client := ec2instanceconnect.NewFromConfig(cfg)

	_, err = client.SendSSHPublicKey(ctx, &ec2instanceconnect.SendSSHPublicKeyInput{
		InstanceId:     aws.String(instanceID),
		InstanceOSUser: aws.String("ec2-user"),
		SSHPublicKey:   aws.String(string(pubKey)),
	})
	return err
}
