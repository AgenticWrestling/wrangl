package placement

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Budget is the hourly cost budget and what is committed against it (ADR 0009 section 8).
type Budget struct {
	LimitUSD  *float64 // nil = no budget
	WarnPct   int64
	Committed float64 // USD per hour: the price of every runner that is not terminated
}

// Approaching is true when a budget is set and committed spend is at or above the warning share of it.
func (b Budget) Approaching() bool {
	return b.LimitUSD != nil && b.Committed >= *b.LimitUSD*float64(b.WarnPct)/100
}

// Pct is committed as a percentage of the budget (0 without one).
func (b Budget) Pct() float64 {
	if b.LimitUSD == nil || *b.LimitUSD == 0 {
		return 0
	}
	return b.Committed / *b.LimitUSD * 100
}

func (b Budget) String() string {
	if b.LimitUSD == nil {
		return "no budget"
	}
	return fmt.Sprintf("$%.2f of $%.2f per hour", b.Committed, *b.LimitUSD)
}

// RearmPct is the share below which the "approaching" note can fire again (hysteresis).
const RearmPct = 70

// MacMinimumHours is how long an EC2 Mac host bills at least (ADR 0009 decision 11).
const MacMinimumHours = 24

// LaunchCommit is what launching one instance of the type commits against the budget: its hourly
// price, except that a Mac host counts 24 hours of it because it cannot be released earlier.
func LaunchCommit(instanceType string, hourly float64) float64 {
	if strings.HasPrefix(instanceType, "mac") {
		return hourly * MacMinimumHours
	}
	return hourly
}

// Budget reads the budget and the committed sum.
func (s *Service) Budget(ctx context.Context) (Budget, error) {
	row, err := s.DB.RQ.GetBudget(ctx)
	if err != nil {
		return Budget{}, err
	}
	b := Budget{WarnPct: row.BudgetWarnPct}
	if row.BudgetHourlyUsd.Valid {
		v := row.BudgetHourlyUsd.Float64
		b.LimitUSD = &v
	}
	b.Committed, err = s.DB.RQ.CommittedHourlyUSD(ctx)
	return b, err
}

// SetBudget sets the hourly budget; an empty or "none" value removes it.
func (s *Service) SetBudget(ctx context.Context, value string) error {
	value = strings.TrimSpace(strings.TrimPrefix(value, "$"))
	if value == "" || strings.EqualFold(value, "none") || value == "0" {
		return s.DB.Q.SetBudget(ctx, sql.NullFloat64{})
	}
	var v float64
	if _, err := fmt.Sscanf(value, "%f", &v); err != nil || v <= 0 {
		return fmt.Errorf("%w: budget %q: want dollars per hour, e.g. 0.50, or none", errs.Invalid, value)
	}
	return s.DB.Q.SetBudget(ctx, sql.NullFloat64{Float64: v, Valid: true})
}

// CheckApproaching applies the hysteresis: note is true when committed spend has just crossed the warning
// share (once per crossing); it re-arms when spend falls below RearmPct.
func (s *Service) CheckApproaching(ctx context.Context) (note bool, b Budget, err error) {
	b, err = s.Budget(ctx)
	if err != nil || b.LimitUSD == nil {
		return false, b, err
	}
	row, err := s.DB.RQ.GetBudget(ctx)
	if err != nil {
		return false, b, err
	}
	switch {
	case b.Approaching() && row.BudgetWarned == 0:
		return true, b, s.DB.Q.SetBudgetWarned(ctx, 1)
	case b.Pct() < RearmPct && row.BudgetWarned == 1:
		return false, b, s.DB.Q.SetBudgetWarned(ctx, 0)
	}
	return false, b, nil
}
