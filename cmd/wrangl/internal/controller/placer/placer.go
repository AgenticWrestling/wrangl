// Package placer is the pure part of resource-based placement (ADR 0009 section 6.2 and 7.3): whether a
// run fits a runner, and in what order the runners that fit are tried. It has no database and no clock.
package placer

import "sort"

// Need is what a run reserves (its queue-time snapshot). 0/0/0 reserves nothing: only the session
// ceiling then limits a runner, exactly as before resources existed.
type Need struct{ VCPU, RAMMiB, DiskGiB int64 }

// Runner is a placement candidate as of one snapshot.
type Runner struct {
	ID int64
	// Claimable: idle or busy, not cordoned or draining, at the template's version, no upgrade pending.
	Claimable bool
	Busy      bool   // state busy (it already hosts sessions)
	IdleSince string // sortable timestamp; "" sorts first
	Cap, Used Need   // capacity and what active runs reserved
	Active    int64  // active runs
	Ceiling   *int64 // optional session ceiling; nil = none
	// HourlyUSD is the runner's price; nil = free (the host).
	HourlyUSD *float64
}

// Free is the capacity left.
func (r Runner) Free() Need {
	return Need{r.Cap.VCPU - r.Used.VCPU, r.Cap.RAMMiB - r.Used.RAMMiB, r.Cap.DiskGiB - r.Used.DiskGiB}
}

// Fits reports whether a run with need n can be placed on r: the runner is claimable, every resource
// the run needs is free (a need of 0 is not checked), and the session ceiling has room.
func Fits(n Need, r Runner) bool {
	if !r.Claimable {
		return false
	}
	f := r.Free()
	if (n.VCPU != 0 && n.VCPU > f.VCPU) || (n.RAMMiB != 0 && n.RAMMiB > f.RAMMiB) || (n.DiskGiB != 0 && n.DiskGiB > f.DiskGiB) {
		return false
	}
	return r.Ceiling == nil || r.Active < *r.Ceiling
}

func price(r Runner) float64 {
	if r.HourlyUSD == nil {
		return 0
	}
	return *r.HourlyUSD
}

// leftover is how much of the runner stays free after placing n, as a fraction of its capacity,
// summed over the resources the run needs (ADR 0009 7.3 "fullest"). A run that needs nothing (the
// 0/0/0 default) has no leftover to compare: every runner ties and busy-then-longest-idle decides,
// which is the order before resources existed.
func leftover(n Need, r Runner) float64 {
	var s float64
	for _, x := range []struct{ free, need, cap int64 }{
		{r.Cap.VCPU - r.Used.VCPU, n.VCPU, r.Cap.VCPU}, {r.Cap.RAMMiB - r.Used.RAMMiB, n.RAMMiB, r.Cap.RAMMiB}, {r.Cap.DiskGiB - r.Used.DiskGiB, n.DiskGiB, r.Cap.DiskGiB},
	} {
		if x.need != 0 && x.cap > 0 {
			s += float64(x.free-x.need) / float64(x.cap)
		}
	}
	return s
}

// Candidates returns the runners n fits, best first: cheapest (the free host first), then fullest
// (smallest leftover, which keeps whole runners empty so they can be reaped), then busy before idle,
// then the longest idle, then lowest id.
func Candidates(n Need, runners []Runner) []Runner {
	var out []Runner
	for _, r := range runners {
		if Fits(n, r) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if pa, pb := price(a), price(b); pa != pb {
			return pa < pb
		}
		if la, lb := leftover(n, a), leftover(n, b); la != lb {
			return la < lb
		}
		if a.Busy != b.Busy {
			return a.Busy
		}
		if a.IdleSince != b.IdleSince {
			return a.IdleSince < b.IdleSince
		}
		return a.ID < b.ID
	})
	return out
}
