package placement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// DefaultNeedsInputGrace is the suggested grace when the owner turns the policy on (ADR 0009 decision 7).
const DefaultNeedsInputGrace = 10 * time.Minute

// NeedsInputGrace is how long a run may wait on the owner before it stops reserving vCPU; 0 = off.
func (s *Service) NeedsInputGrace(ctx context.Context) time.Duration {
	n, err := s.DB.RQ.GetNeedsInputGrace(ctx)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// NeedsInputCutoff is the timestamp a question must predate for its run to have released its vCPU: now
// minus the grace, or "" when the policy is off (nothing predates "").
func (s *Service) NeedsInputCutoff(ctx context.Context) string {
	g := s.NeedsInputGrace(ctx)
	if g == 0 {
		return ""
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	return dbtime.Format(now.Add(-g))
}

// SetNeedsInputGrace sets the grace ("10m"); "off" or "0" turns the policy off.
func (s *Service) SetNeedsInputGrace(ctx context.Context, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || value == "off" || value == "0" {
		return s.DB.Q.SetNeedsInputGrace(ctx, 0)
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < time.Minute {
		return fmt.Errorf("%w: needs-input grace %q: want a duration of at least 1m (10m), or off", errs.Invalid, value)
	}
	return s.DB.Q.SetNeedsInputGrace(ctx, int64(d/time.Second))
}
