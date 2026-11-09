package placer

import "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/capability"

// Run is a queued run to place, in queue order (project priority, queue rank, rank, queued time).
type Run struct {
	ID      int64
	Project int64
	// Template is the template whose runners host the run's agent session (for a remote template, its
	// driver). Allowed lists other templates whose runners it may use first (the project's host).
	Template int64
	Allowed  []int64
	Need     Need
	// Reqs are the run's capability requirements (snapshotted when it was queued).
	Reqs []capability.Requirement
}

// RunnerIn is an existing runner as of one snapshot: Runner plus where it lives. Arriving runners are
// still launching or enrolling: runs may be reserved on them (and wait) but not assigned yet.
type RunnerIn struct {
	Runner
	Template int64
	Arriving bool
}

// Entry is something a template can launch: one menu entry (or the template itself when it has none).
type Entry struct {
	Template int64
	Type     string          // "" = the template's own choice (legacy)
	Cap      Need            // what sessions can use on a fresh runner of this type
	Tags     capability.Tags // the capabilities a runner of this entry has (its template's)
	Ceiling  int64           // session ceiling (the template's max_sessions)
	// HourlyUSD is the type's price; nil = unknown (only matters once a budget exists).
	HourlyUSD *float64
	// Volume sizing: the runner's disk is max(MinDiskGiB, hosted disk needs + ReserveDiskGiB).
	MinDiskGiB, ReserveDiskGiB int64
	DiskUSDPerGiBHour          float64
}

// Limits bound what may be launched this pass.
type Limits struct {
	// Room is how many more runners each template may launch (max_runners minus live; 0 when launching
	// is blocked: failed version, backoff, enrol brake).
	Room map[int64]int64
	// Budget, when set, caps what the plan may launch: committed spend plus a launch's price must stay
	// within Limit (ADR 0009 section 8.2). Entries without a known price are not launched then.
	Budget *Budget
}

// Budget is the hourly cost ceiling and what is already committed (USD per hour).
type Budget struct{ Limit, Committed float64 }

// Assignment puts a run on a runner. In Plan.Wait the runner is still arriving, so the run stays
// queued; Runner is then the runner's ID, or -n for the n-th launch of the plan.
type Assignment struct{ Run, Runner int64 }

// Launch is a runner to start and the runs the plan hosts on it.
type Launch struct {
	Entry   Entry
	Project int64 // the project of the first run it was planned for
	Runs    []int64
	// DiskGiB is the volume to ask for: the template minimum, or hosted disk needs plus reserve.
	DiskGiB int64
}

// Blocked is a run that cannot be placed now, and why.
type Blocked struct {
	Run  int64
	Code string
	// For CodeBudget: the cheapest entry that would have fit but for the budget, what it would add,
	// and the budget figures.
	Type                   string
	Adds, Committed, Limit float64
}

// Reason codes of Blocked.
const (
	CodeCapacity     = "capacity"      // cannot launch: the template has no room (max_runners)
	CodeTooBig       = "too_big"       // no entry of its template could ever hold it
	CodeNoCapability = "no_capability" // no entry of its template provides a required capability
	CodeBudget       = "budget"        // launching would exceed the hourly budget
	CodePrice        = "price_unknown" // a budget is set and no entry that could hold it has a known price
)

// Plan is the result of one pass.
type Plan struct {
	Assign      []Assignment // run -> existing claimable runner (Runner is the input runner's ID)
	Wait        []Assignment // run -> a runner that is arriving
	Launch      []Launch
	Unplaceable []Blocked
}

type virt struct {
	RunnerIn
	id     int64
	launch int // index into Plan.Launch for a runner planned this pass, -1 otherwise
	entry  Entry
}

func (v *virt) reserve(n Need) {
	v.Used.VCPU += n.VCPU
	v.Used.RAMMiB += n.RAMMiB
	v.Used.DiskGiB += n.DiskGiB
	v.Active++
}

func entryRunner(e Entry) Runner {
	c := e.Ceiling
	return Runner{Claimable: true, Cap: e.Cap, Ceiling: &c, Tags: e.Tags}
}

// commit is what launching the entry commits: its price, 24 hours of it for an EC2 Mac host.
func (e Entry) commit(hostedDisk int64) float64 {
	p := e.price(hostedDisk)
	if len(e.Type) >= 3 && e.Type[:3] == "mac" {
		return p * 24
	}
	return p
}

func (e Entry) volume(hostedDisk int64) int64 { return max(e.MinDiskGiB, hostedDisk+e.ReserveDiskGiB) }

// Price is the hourly price of a runner of this entry with a volume sized for hostedDisk GiB of needs; false when
// the instance price is unknown.
func (e Entry) Price(hostedDisk int64) (float64, bool) {
	return e.price(hostedDisk), e.HourlyUSD != nil
}

func (e Entry) price(hostedDisk int64) float64 {
	p := 0.0
	if e.HourlyUSD != nil {
		p = *e.HourlyUSD
	}
	return p + float64(e.volume(hostedDisk))*e.DiskUSDPerGiBHour
}

// Place decides, for the queued runs in order, which existing runner takes each, which arriving
// runner each waits for, and which runners to launch. It reserves as it goes on virtual copies, so a
// later run never takes room an earlier one was entitled to and one launch is credited with every
// queued run it can host (ADR 0009 sections 7.2 and 7.4). A launch is chosen by the lowest
// price per hosted run, then more hosted runs, then the lower price, then the type name.
func Place(runs []Run, runners []RunnerIn, entries []Entry, lim Limits) Plan {
	var plan Plan
	vs := make([]*virt, len(runners))
	for i, r := range runners {
		vs[i] = &virt{RunnerIn: r, id: r.ID, launch: -1}
	}
	room := map[int64]int64{}
	for k, v := range lim.Room {
		room[k] = v
	}
	committed := 0.0
	if lim.Budget != nil {
		committed = lim.Budget.Committed
	}
	byTemplate := map[int64][]Entry{}
	for _, e := range entries {
		byTemplate[e.Template] = append(byTemplate[e.Template], e)
	}

	// pick finds the best virtual runner of one template (claimable or arriving) that r fits.
	pick := func(r Run, tmpl int64, arriving bool) *virt {
		var pool []Runner
		by := map[int64]*virt{}
		for i, v := range vs {
			if v.Template != tmpl || v.Arriving != arriving {
				continue
			}
			rr := v.Runner
			rr.ID = int64(i) + 1 // position, so planned runners (no id) are addressable
			rr.Claimable = true  // arriving runners count; claimable ones were already filtered by the caller
			if !arriving {
				rr.Claimable = v.Runner.Claimable
			}
			pool = append(pool, rr)
			by[rr.ID] = v
		}
		if c := Candidates(r.Need, pool, r.Reqs...); len(c) > 0 {
			return by[c[0].ID]
		}
		return nil
	}

	for i, r := range runs {
		order := append(append([]int64{}, r.Allowed...), r.Template) // the host first, then its own
		placed := false
		for _, t := range order { // 1. an existing claimable runner
			if v := pick(r, t, false); v != nil {
				v.reserve(r.Need)
				plan.Assign = append(plan.Assign, Assignment{r.ID, v.id})
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		for _, t := range order { // 2. a runner that is arriving (launching, or planned this pass)
			if v := pick(r, t, true); v != nil {
				v.reserve(r.Need)
				if v.launch >= 0 {
					l := &plan.Launch[v.launch]
					l.Runs = append(l.Runs, r.ID)
					l.DiskGiB = v.entry.volume(v.Used.DiskGiB)
				}
				plan.Wait = append(plan.Wait, Assignment{r.ID, v.id})
				placed = true
				break
			}
		}
		if placed {
			continue
		}
		// 3. launch.
		var holders []Entry
		sized := false // some entry is big enough, whatever it provides
		for _, e := range byTemplate[r.Template] {
			if Fits(r.Need, entryRunner(e)) {
				sized = true
				if capability.Match(r.Reqs, e.Tags) {
					holders = append(holders, e)
				}
			}
		}
		if len(holders) == 0 {
			code := CodeTooBig
			if sized {
				code = CodeNoCapability
			}
			plan.Unplaceable = append(plan.Unplaceable, Blocked{Run: r.ID, Code: code})
			continue
		}
		if room[r.Template] <= 0 {
			plan.Unplaceable = append(plan.Unplaceable, Blocked{Run: r.ID, Code: CodeCapacity})
			continue
		}
		if lim.Budget != nil { // the budget gate: only entries with a known price that fit under the ceiling
			var fit []Entry
			cheapest, cheapestAdds := "", 0.0
			priced := false
			for _, e := range holders {
				if e.HourlyUSD == nil {
					continue
				}
				priced = true
				_, disk := pack(e, r, runs[i+1:])
				adds := e.commit(disk)
				if committed+adds <= lim.Budget.Limit {
					fit = append(fit, e)
				} else if cheapest == "" || adds < cheapestAdds {
					cheapest, cheapestAdds = e.Type, adds
				}
			}
			if len(fit) == 0 {
				code := CodeBudget
				if !priced {
					code = CodePrice
				}
				plan.Unplaceable = append(plan.Unplaceable, Blocked{Run: r.ID, Code: code, Type: cheapest, Adds: cheapestAdds, Committed: committed, Limit: lim.Budget.Limit})
				continue
			}
			holders = fit
		}
		best, bestHosted, bestScore, bestPrice := -1, 0, 0.0, 0.0
		for j, e := range holders {
			hosted, disk := pack(e, r, runs[i+1:])
			price := e.price(disk)
			score := price / float64(hosted)
			if best < 0 || score < bestScore || (score == bestScore && (hosted > bestHosted ||
				(hosted == bestHosted && (price < bestPrice || (price == bestPrice && e.Type < holders[best].Type))))) {
				best, bestHosted, bestScore, bestPrice = j, hosted, score, price
			}
		}
		e := holders[best]
		if lim.Budget != nil {
			_, disk := pack(e, r, runs[i+1:])
			committed += e.commit(disk)
		}
		room[r.Template]--
		plan.Launch = append(plan.Launch, Launch{Entry: e, Project: r.Project, Runs: []int64{r.ID}, DiskGiB: e.volume(r.Need.DiskGiB)})
		nv := &virt{RunnerIn: RunnerIn{Runner: entryRunner(e), Template: e.Template, Arriving: true}, id: -int64(len(plan.Launch)), launch: len(plan.Launch) - 1, entry: e}
		nv.reserve(r.Need)
		vs = append(vs, nv)
		plan.Wait = append(plan.Wait, Assignment{r.ID, nv.id})
	}
	return plan
}

// pack simulates hosting r and then the later runs of the same template on one fresh runner of entry e,
// first-fit in queue order, and returns how many it hosts and their total disk need.
func pack(e Entry, r Run, later []Run) (hosted int, disk int64) {
	v := virt{RunnerIn: RunnerIn{Runner: entryRunner(e), Template: e.Template}}
	v.reserve(r.Need)
	hosted, disk = 1, r.Need.DiskGiB
	for _, o := range later {
		if o.Template == r.Template && Fits(o.Need, v.Runner) {
			v.reserve(o.Need)
			hosted++
			disk += o.Need.DiskGiB
		}
	}
	return hosted, disk
}
