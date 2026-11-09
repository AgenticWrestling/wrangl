package awsx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ktr0731/go-fuzzyfinder"
)

// PickProfile asks the user to choose an AWS profile with a fuzzy finder.
func PickProfile(ctx context.Context, profiles []string) (string, error) {
	if len(profiles) == 0 {
		return "", fmt.Errorf("no AWS profiles found in ~/.aws/config (pass --profile or set AWS_PROFILE)")
	}
	infos := make([]ProfileInfo, len(profiles))
	var wg sync.WaitGroup
	for i, p := range profiles {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			infos[i] = LoadProfileInfo(ctx, p)
		}(i, p)
	}
	wg.Wait()
	idx, err := fuzzyfinder.Find(infos, func(i int) string {
		in := infos[i]
		parts := []string{fmt.Sprintf("%-30s", in.Name)}
		for _, f := range []struct {
			v string
			w int
		}{{in.AccountID, 14}, {in.OrgHint, 16}, {in.Role, 24}, {in.Region, 14}} {
			if f.v != "" {
				parts = append(parts, fmt.Sprintf("%-*s", f.w, f.v))
			}
		}
		if in.IsSSO {
			if in.TokenOK {
				parts = append(parts, "✓")
			} else {
				parts = append(parts, "⚠")
			}
		}
		return strings.Join(parts, "  ")
	})
	if err != nil {
		if errors.Is(err, fuzzyfinder.ErrAbort) {
			return "", errors.New("cancelled")
		}
		return "", err
	}
	return infos[idx].Name, nil
}

// PickInstance asks the user to choose an instance with a fuzzy finder.
func PickInstance(instances []Instance) (*Instance, error) {
	if len(instances) == 0 {
		return nil, errors.New("no matching instances found")
	}
	idx, err := fuzzyfinder.Find(instances, func(i int) string {
		in := instances[i]
		return fmt.Sprintf("%-30s  %-20s  %-9s  %s", in.Name, in.ID, in.State, in.PrivateIP)
	})
	if err != nil {
		if errors.Is(err, fuzzyfinder.ErrAbort) {
			return nil, errors.New("cancelled")
		}
		return nil, err
	}
	return &instances[idx], nil
}
