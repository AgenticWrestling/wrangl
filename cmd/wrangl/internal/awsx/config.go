// Package awsx holds the AWS plumbing shared by `wrangl dev`, `wrangl fleet connect` and the
// runner: profile and region resolution, SSO, instance lookup, EC2 Instance Connect, command
// execution over SSM and port-forward tunnels. Nothing here needs a controller.
package awsx

import (
	"context"
	"errors"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// LoadConfig loads the AWS config for a named profile (empty: the default chain) and region
// (empty: AWS_REGION, then the profile's region). It fails if no region results.
func LoadConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return cfg, err
	}
	if cfg.Region == "" {
		return cfg, errors.New("no AWS region: pass --region or set AWS_REGION")
	}
	return cfg, nil
}

// ProfileRegion returns the region configured for a profile ("" if none).
func ProfileRegion(profile string) string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	if r := os.Getenv("AWS_DEFAULT_REGION"); r != "" {
		return r
	}
	return profileKey(profile, "region")
}
