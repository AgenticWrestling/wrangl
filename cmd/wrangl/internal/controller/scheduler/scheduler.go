// Package scheduler decides when to launch runners. M3 does scale-up from queued runs;
// assigning runs to runners arrives with M4.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/fleet"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/gh"
)

// Backoff after a failed launch, per template: doubles from MinBackoff to MaxBackoff.
const (
	MinBackoff = time.Minute
	MaxBackoff = 30 * time.Minute
)

type backoff struct {
	until time.Time
	next  time.Duration
}

// Scheduler launches runners to meet queued demand, within each template's max_runners.
type Scheduler struct {
	Fleet  *fleet.Service
	Now    func() time.Time
	Log    *slog.Logger
	GH     gh.API                            // nil: no tokens (file:// repos in tests)
	Secret func(name string) ([]byte, error) // controller secret store

	mu      sync.Mutex
	backoff map[int64]*backoff
	warned  map[int64]int64 // template id -> failed version already logged
}

// Tick runs one scale-up pass and returns how many runners it launched per template.
func (s *Scheduler) Tick(ctx context.Context) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff == nil {
		s.backoff, s.warned = map[int64]*backoff{}, map[int64]int64{}
	}
	rows, err := s.Fleet.DB.RQ.TemplateDemand(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	launched := map[string]int{}
	var errs []error
	for _, t := range rows {
		need := t.Demand - t.Supply
		room := t.MaxRunners - t.Live
		n := min(need, room)
		if n <= 0 {
			continue
		}
		if t.FailedVersion == t.Version {
			if s.warned[t.ID] != t.Version {
				s.Log.Warn("not launching: template's current version failed to apply; fix it with `wrangl template edit`",
					"template", t.Name, "version", t.Version, "queued", t.Demand)
				s.warned[t.ID] = t.Version
			}
			continue
		}
		b := s.backoff[t.ID]
		if b != nil && now.Before(b.until) {
			continue
		}
		for i := int64(0); i < n; i++ {
			if _, err := s.Fleet.Launch(ctx, t.Name); err != nil {
				next := MinBackoff
				if b != nil {
					next = min(b.next*2, MaxBackoff)
				}
				s.backoff[t.ID] = &backoff{until: now.Add(next), next: next}
				s.Log.Error("launch failed; backing off", "template", t.Name, "retry_in", next, "err", err)
				errs = append(errs, err)
				break
			}
			delete(s.backoff, t.ID)
			launched[t.Name]++
		}
	}
	return launched, errors.Join(errs...)
}

// Loop ticks every interval until ctx ends.
func (s *Scheduler) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Log.Debug("scheduler tick", "err", err)
		}
		if s.Fleet.NC != nil {
			if _, err := s.Assign(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("assign", "err", err)
			}
		}
	}
}
