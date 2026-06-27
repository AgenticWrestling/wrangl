// cmd/ssm/internal/ui/picker.go
package ui

import (
	"errors"
	"fmt"

	"github.com/ktr0731/go-fuzzyfinder"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

func PickProfile(profiles []string) (string, error) {
	if len(profiles) == 0 {
		return "", fmt.Errorf("no AWS profiles found in ~/.aws/config")
	}
	idx, err := fuzzyfinder.Find(profiles, func(i int) string { return profiles[i] })
	if err != nil {
		if errors.Is(err, fuzzyfinder.ErrAbort) {
			return "", fmt.Errorf("cancelled")
		}
		return "", err
	}
	return profiles[idx], nil
}

func PickInstance(instances []ssmaws.Instance) (*ssmaws.Instance, error) {
	if len(instances) == 0 {
		return nil, fmt.Errorf("no running instances found")
	}
	idx, err := fuzzyfinder.Find(instances, func(i int) string {
		inst := instances[i]
		return fmt.Sprintf("%-30s  %-20s  %s", inst.Name, inst.ID, inst.PrivateIP)
	})
	if err != nil {
		if errors.Is(err, fuzzyfinder.ErrAbort) {
			return nil, fmt.Errorf("cancelled")
		}
		return nil, err
	}
	return &instances[idx], nil
}
