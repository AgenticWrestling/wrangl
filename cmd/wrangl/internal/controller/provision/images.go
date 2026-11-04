package provision

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// Image is an AMI offered through the AWS Marketplace.
type Image struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Arch        string `json:"arch"` // arm64 | x86_64
	Description string `json:"description,omitempty"`
}

// SearchMarketplace lists Marketplace AMIs in this region whose name contains q, newest names
// first, at most 60. arch (arm64 | x86_64) narrows it; "" is both.
func (p *Provisioner) SearchMarketplace(ctx context.Context, q, arch string) ([]Image, error) {
	q = strings.TrimSpace(q)
	if len(q) < 3 {
		return nil, errors.New("type at least three letters of the product name")
	}
	if strings.ContainsAny(q, "*?") {
		return nil, errors.New("wildcards are not needed")
	}
	filters := []types.Filter{
		{Name: aws.String("name"), Values: []string{"*" + q + "*"}},
		{Name: aws.String("state"), Values: []string{"available"}},
	}
	if arch != "" {
		filters = append(filters, types.Filter{Name: aws.String("architecture"), Values: []string{arch}})
	}
	out, err := p.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"aws-marketplace"}, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("DescribeImages: %w", err)
	}
	var res []Image
	for _, im := range out.Images {
		res = append(res, Image{ID: aws.ToString(im.ImageId), Name: aws.ToString(im.Name), Arch: string(im.Architecture), Description: aws.ToString(im.Description)})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name > res[j].Name })
	if len(res) > 60 {
		res = res[:60]
	}
	return res, nil
}

// LaunchCheck is the result of asking EC2 whether an image could be launched, without launching.
type LaunchCheck struct {
	OK bool `json:"ok"`
	// OptInRequired: a Marketplace image whose terms this account has not accepted yet.
	OptInRequired bool   `json:"opt_in_required,omitempty"`
	SubscribeURL  string `json:"subscribe_url,omitempty"`
	Error         string `json:"error,omitempty"`
}

var subscribeURL = regexp.MustCompile(`https://aws\.amazon\.com/marketplace/[^\s"']+`)

// CheckLaunch dry-runs RunInstances for the image: EC2 answers "would succeed" or says what is
// wrong (the terms of a Marketplace product not accepted, the image not shared with the account,
// the instance type not supported by the image, ...). Nothing is launched or billed.
func (p *Provisioner) CheckLaunch(ctx context.Context, ami, itype, subnet, sg string) LaunchCheck {
	_, err := p.EC2.RunInstances(ctx, &ec2.RunInstancesInput{
		DryRun: aws.Bool(true), ImageId: aws.String(ami), InstanceType: types.InstanceType(itype),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		SubnetId: aws.String(subnet), SecurityGroupIds: []string{sg},
	})
	var ae smithy.APIError
	switch {
	case err == nil:
		return LaunchCheck{OK: true}
	case errors.As(err, &ae) && ae.ErrorCode() == "DryRunOperation":
		return LaunchCheck{OK: true}
	case errors.As(err, &ae) && ae.ErrorCode() == "OptInRequired":
		return LaunchCheck{OptInRequired: true, SubscribeURL: subscribeURL.FindString(ae.ErrorMessage()), Error: ae.ErrorMessage()}
	case errors.As(err, &ae):
		return LaunchCheck{Error: ae.ErrorCode() + ": " + ae.ErrorMessage()}
	}
	return LaunchCheck{Error: err.Error()}
}
