package scheduler

import (
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

type (
	ec2RunInput  = ec2.RunInstancesInput
	ec2RunOutput = ec2.RunInstancesOutput
	ec2Options   = ec2.Options
)

var errInsufficientCapacity = errors.New("InsufficientInstanceCapacity")
