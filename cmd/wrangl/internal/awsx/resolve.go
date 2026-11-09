package awsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Target is a parsed `[region/][user@]name` argument; a bare instance id sets ID.
type Target struct {
	Region, User, Name, ID string
}

// ParseTarget parses "[region/][user@]name" or an instance id. A lone argument that looks
// like a region ("us-west-2") is a region with no name (pick from that region).
func ParseTarget(arg string) Target {
	var t Target
	if arg == "" {
		return t
	}
	rest := arg
	if r, n, ok := strings.Cut(arg, "/"); ok {
		t.Region, rest = r, n
	} else if LooksLikeRegion(arg) {
		t.Region = arg
		return t
	}
	if u, n, ok := strings.Cut(rest, "@"); ok {
		t.User, rest = u, n
	}
	if IsInstanceID(rest) {
		t.ID = rest
	}
	t.Name = rest
	return t
}

// Resolver turns a target argument into a Session and an Instance, asking the user (fuzzy
// finder) for whatever the flags, the environment and the argument leave open.
type Resolver struct {
	Profile, Region string
	Out             io.Writer // progress messages; default stderr
	// Seams for tests; the defaults use the real AWS SDK and the fuzzy finder.
	NewSession   func(ctx context.Context, profile, region string) (*Session, error)
	PickProfile  func(ctx context.Context, profiles []string) (string, error)
	PickInstance func([]Instance) (*Instance, error)
	EnsureSSO    func(profile string) error
	ListProfiles func() ([]string, error)
	RecordUsage  func(profile string) error
	SortProfiles func([]string) []string
}

func (r *Resolver) fill() {
	if r.Out == nil {
		r.Out = os.Stderr
	}
	if r.NewSession == nil {
		r.NewSession = NewSession
	}
	if r.PickProfile == nil {
		r.PickProfile = PickProfile
	}
	if r.PickInstance == nil {
		r.PickInstance = PickInstance
	}
	if r.EnsureSSO == nil {
		r.EnsureSSO = EnsureSSO
	}
	if r.ListProfiles == nil {
		r.ListProfiles = ListProfiles
	}
	if r.RecordUsage == nil {
		r.RecordUsage = RecordProfileUsage
	}
	if r.SortProfiles == nil {
		r.SortProfiles = SortProfilesByMRU
	}
}

// ChooseProfile returns --profile, else $AWS_PROFILE, else asks.
func (r *Resolver) ChooseProfile(ctx context.Context) (string, error) {
	r.fill()
	p := r.Profile
	if p == "" {
		p = os.Getenv("AWS_PROFILE")
	}
	if p == "" {
		ps, err := r.ListProfiles()
		if err != nil {
			return "", fmt.Errorf("listing profiles: %w", err)
		}
		if p, err = r.PickProfile(ctx, r.SortProfiles(ps)); err != nil {
			return "", err
		}
	}
	_ = r.RecordUsage(p)
	return p, nil
}

// Session opens a session for the profile and the region (flag > argument > env > profile).
func (r *Resolver) Session(ctx context.Context, argRegion string) (*Session, string, error) {
	profile, err := r.ChooseProfile(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := r.EnsureSSO(profile); err != nil {
		return nil, "", err
	}
	region := r.Region
	if region == "" {
		region = argRegion
	}
	if region == "" {
		region = ProfileRegion(profile)
	}
	s, err := r.NewSession(ctx, profile, region)
	return s, profile, err
}

// Resolve finds the instance for arg (empty: pick from the region). states filter what may be
// picked or matched by name; default running. A name with no match falls back to the picker.
func (r *Resolver) Resolve(ctx context.Context, arg string, states ...string) (*Session, *Instance, Target, error) {
	r.fill()
	t := ParseTarget(arg)
	s, _, err := r.Session(ctx, t.Region)
	if err != nil {
		return nil, nil, t, err
	}
	find := func() (*Instance, error) {
		if t.ID != "" {
			return s.Get(ctx, t.ID)
		}
		if t.Name != "" {
			i, err := s.ByName(ctx, t.Name, states...)
			if err == nil || IsSSOExpired(err) {
				return i, err
			}
			fmt.Fprintf(r.Out, "%v: pick one:\n", err)
		}
		if len(states) == 0 {
			states = []string{"running"}
		}
		l, err := s.List(ctx, states...)
		if err != nil {
			return nil, err
		}
		return r.PickInstance(l)
	}
	i, err := find()
	if IsSSOExpired(err) {
		fmt.Fprintf(r.Out, "SSO session expired for profile %q. Refreshing...\n", s.Profile)
		if rerr := RefreshSSO(s.Profile); rerr != nil {
			return nil, nil, t, fmt.Errorf("sso login: %w", rerr)
		}
		i, err = find()
	}
	if err != nil {
		return nil, nil, t, err
	}
	if i == nil {
		return nil, nil, t, errors.New("no instance selected")
	}
	return s, i, t, nil
}
