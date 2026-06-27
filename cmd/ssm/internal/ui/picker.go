// cmd/ssm/internal/ui/picker.go
package ui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ktr0731/go-fuzzyfinder"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

func PickProfile(profiles []string) (string, error) {
	if len(profiles) == 0 {
		return "", fmt.Errorf("no AWS profiles found in ~/.aws/config")
	}

	infos := make([]ssmaws.ProfileInfo, len(profiles))
	for i, p := range profiles {
		infos[i] = ssmaws.LoadProfileInfo(p)
	}

	idx, err := fuzzyfinder.Find(infos, func(i int) string {
		info := infos[i]
		status := ""
		if info.IsSSO {
			if info.TokenOK {
				status = "✓"
			} else {
				status = "⚠"
			}
		}
		parts := []string{fmt.Sprintf("%-30s", info.Name)}
		if info.AccountID != "" {
			parts = append(parts, fmt.Sprintf("%-14s", info.AccountID))
		}
		if info.OrgHint != "" {
			parts = append(parts, fmt.Sprintf("%-16s", info.OrgHint))
		}
		if info.Role != "" {
			parts = append(parts, fmt.Sprintf("%-24s", info.Role))
		}
		if info.Region != "" {
			parts = append(parts, fmt.Sprintf("%-14s", info.Region))
		}
		if status != "" {
			parts = append(parts, status)
		}
		return strings.Join(parts, "  ")
	})
	if err != nil {
		if errors.Is(err, fuzzyfinder.ErrAbort) {
			return "", fmt.Errorf("cancelled")
		}
		return "", err
	}
	return infos[idx].Name, nil
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
