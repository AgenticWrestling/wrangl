// Package costs answers "what is this AWS account costing": the hourly rate of everything
// running now (every region), the month-to-date bill and a projection to the end of the
// month (Cost Explorer), the instances that are not wrangl runners, and a curated instance
// type catalogue with prices for the template editor. Everything is read-only, cached and
// refreshed in the background so a page never waits on AWS.
package costs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// PricingAPI, CostAPI and EC2API are the slices of the AWS clients used here (faked in tests).
type PricingAPI interface {
	GetProducts(context.Context, *pricing.GetProductsInput, ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

type CostAPI interface {
	GetCostAndUsage(context.Context, *costexplorer.GetCostAndUsageInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
	GetCostForecast(context.Context, *costexplorer.GetCostForecastInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostForecastOutput, error)
}

type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// Service holds the AWS clients and the caches.
type Service struct {
	Pricing PricingAPI
	Cost    CostAPI
	// EC2 returns a client for a region; Regions lists the regions to scan.
	EC2     func(region string) EC2API
	Regions func(ctx context.Context) ([]string, error)
	Home    string // the controller's region: catalogue specs and prices are read here
	Log     *slog.Logger
	Now     func() time.Time

	mu     sync.Mutex
	prices map[string]priceEntry
	snap   Snapshot
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Instance is one EC2 instance anywhere in the account.
type Instance struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Region     string            `json:"region"`
	AZ         string            `json:"az"`
	Type       string            `json:"type"`
	State      string            `json:"state"`
	Arch       string            `json:"arch,omitempty"` // arm64 | x86_64
	PrivateIP  string            `json:"private_ip,omitempty"`
	VPC        string            `json:"vpc,omitempty"`
	LaunchedAt time.Time         `json:"launched_at"`
	HourlyUSD  float64           `json:"hourly_usd"`           // 0 unless running
	PriceKnown bool              `json:"price_known"`          // false: no rate found for this type/region
	Indicative bool              `json:"indicative,omitempty"` // the rate is the built-in table's, not AWS's
	Managed    bool              `json:"managed"`              // launched by a wrangl controller
	Tags       map[string]string `json:"tags,omitempty"`
}

// Summary is the spend overview.
type Summary struct {
	HourlyUSD       float64 `json:"hourly_usd"` // running instances, all regions, EC2 compute only
	Running         int     `json:"running"`
	UnpricedRunning int     `json:"unpriced_running"`  // running instances with no known rate (not in HourlyUSD)
	MonthToDateUSD  float64 `json:"month_to_date_usd"` // whole account, all services
	ForecastRestUSD float64 `json:"forecast_rest_usd"`
	ProjectedUSD    float64 `json:"projected_usd"` // month-to-date + forecast for the rest of the month
	CostAvailable   bool    `json:"cost_available"`
	CostError       string  `json:"cost_error,omitempty"`
	Month           string  `json:"month"`
}

// Snapshot is everything the runners page shows, as of AsOf.
type Snapshot struct {
	AsOf      time.Time  `json:"as_of"`
	Summary   Summary    `json:"summary"`
	Instances []Instance `json:"instances"`
	Errors    []string   `json:"errors,omitempty"` // regions that could not be read
}

// Terminate terminates one instance in a region and drops it from the cached snapshot so
// the page updates at once. Callers decide whether the instance may be terminated.
func (s *Service) Terminate(ctx context.Context, region, id string) error {
	if _, err := s.EC2(region).TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.snap.Instances {
		if s.snap.Instances[i].ID == id && s.snap.Instances[i].Region == region {
			s.snap.Instances[i].State = "shutting-down"
			s.snap.Instances[i].HourlyUSD = 0
		}
	}
	s.mu.Unlock()
	return nil
}

// Cached returns the last refresh (zero AsOf: none yet).
func (s *Service) Cached() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// Loop refreshes the snapshot every interval until ctx ends. Cost Explorer bills a cent per
// request, so its numbers are refreshed at most every costEvery.
func (s *Service) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("cost refresh", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const costEvery = 6 * time.Hour

// Refresh rebuilds the snapshot.
func (s *Service) Refresh(ctx context.Context) error {
	insts, errs := s.inventory(ctx)
	sum := Summary{Month: s.now().Format("2006-01")}
	for _, in := range insts {
		if in.State == "running" {
			sum.Running++
			if in.PriceKnown {
				sum.HourlyUSD += in.HourlyUSD
			} else {
				sum.UnpricedRunning++
			}
		}
	}
	s.mu.Lock()
	prev := s.snap
	s.mu.Unlock()
	if prev.Summary.CostAvailable && s.now().Sub(prev.AsOf) < costEvery && prev.Summary.Month == sum.Month {
		sum.MonthToDateUSD, sum.ForecastRestUSD, sum.ProjectedUSD, sum.CostAvailable = prev.Summary.MonthToDateUSD, prev.Summary.ForecastRestUSD, prev.Summary.ProjectedUSD, true
		prev.Summary = sum
		s.mu.Lock()
		s.snap = Snapshot{AsOf: prev.AsOf, Summary: sum, Instances: insts, Errors: errs}
		s.mu.Unlock()
		return nil
	}
	mtd, rest, cerr := s.monthBill(ctx)
	if cerr != nil {
		sum.CostError = cerr.Error()
	} else {
		sum.MonthToDateUSD, sum.ForecastRestUSD, sum.ProjectedUSD, sum.CostAvailable = mtd, rest, mtd+rest, true
	}
	s.mu.Lock()
	s.snap = Snapshot{AsOf: s.now(), Summary: sum, Instances: insts, Errors: errs}
	s.mu.Unlock()
	return nil
}

// monthBill returns the account's month-to-date spend (all services) and Cost Explorer's
// forecast for the rest of the month.
func (s *Service) monthBill(ctx context.Context) (mtd, rest float64, err error) {
	if s.Cost == nil {
		return 0, 0, fmt.Errorf("cost explorer is not configured")
	}
	now := s.now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := first.AddDate(0, 1, 0)
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	if now.After(first.Add(24 * time.Hour)) { // the API wants End > Start; data lags by up to a day
		out, err := s.Cost.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
			TimePeriod:  &cetypes.DateInterval{Start: aws.String(day(first)), End: aws.String(day(now))},
			Granularity: cetypes.GranularityMonthly,
			Metrics:     []string{"UnblendedCost"},
		})
		if err != nil {
			return 0, 0, fmt.Errorf("cost explorer: %w", err)
		}
		for _, r := range out.ResultsByTime {
			if m, ok := r.Total["UnblendedCost"]; ok {
				v, _ := strconv.ParseFloat(aws.ToString(m.Amount), 64)
				mtd += v
			}
		}
	}
	if next.Sub(now) > 24*time.Hour {
		out, err := s.Cost.GetCostForecast(ctx, &costexplorer.GetCostForecastInput{
			TimePeriod:  &cetypes.DateInterval{Start: aws.String(day(now)), End: aws.String(day(next))},
			Metric:      cetypes.MetricUnblendedCost,
			Granularity: cetypes.GranularityMonthly,
		})
		if err != nil {
			// Forecasting needs some history; the bill so far is still worth showing.
			return mtd, 0, fmt.Errorf("cost forecast: %w", err)
		}
		if out.Total != nil {
			rest, _ = strconv.ParseFloat(aws.ToString(out.Total.Amount), 64)
		}
	}
	return mtd, rest, nil
}

// --- inventory ----------------------------------------------------------------------------------

func (s *Service) inventory(ctx context.Context) ([]Instance, []string) {
	regions, err := s.Regions(ctx)
	if err != nil {
		return nil, []string{"regions: " + err.Error()}
	}
	var mu sync.Mutex
	var out []Instance
	var errs []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, r := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			list, err := s.region(ctx, region)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, region+": "+err.Error())
				return
			}
			out = append(out, list...)
		}(r)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "running"
		}
		if out[i].HourlyUSD != out[j].HourlyUSD {
			return out[i].HourlyUSD > out[j].HourlyUSD
		}
		return out[i].Region+out[i].ID < out[j].Region+out[j].ID
	})
	sort.Strings(errs)
	return out, errs
}

func (s *Service) region(ctx context.Context, region string) ([]Instance, error) {
	c := s.EC2(region)
	var out []Instance
	in := &ec2.DescribeInstancesInput{Filters: []types.Filter{{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}}}}
	for {
		page, err := c.DescribeInstances(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				x := Instance{ID: aws.ToString(i.InstanceId), Region: region, Type: string(i.InstanceType), Arch: string(i.Architecture), PrivateIP: aws.ToString(i.PrivateIpAddress),
					VPC: aws.ToString(i.VpcId), Tags: map[string]string{}}
				if i.State != nil {
					x.State = string(i.State.Name)
				}
				if i.Placement != nil {
					x.AZ = aws.ToString(i.Placement.AvailabilityZone)
				}
				if i.LaunchTime != nil {
					x.LaunchedAt = i.LaunchTime.UTC()
				}
				for _, t := range i.Tags {
					k, v := aws.ToString(t.Key), aws.ToString(t.Value)
					x.Tags[k] = v
					if k == "Name" {
						x.Name = v
					}
				}
				x.Managed = x.Tags["wrangl:managed"] == "true"
				if x.State == "running" || x.State == "pending" {
					x.HourlyUSD, x.PriceKnown, x.Indicative = s.Price(ctx, region, x.Type)
				} else {
					x.PriceKnown = true // a stopped instance costs no compute
				}
				out = append(out, x)
			}
		}
		if page.NextToken == nil {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}

// --- prices -------------------------------------------------------------------------------------

type priceEntry struct {
	usd        float64
	ok         bool
	indicative bool
	at         time.Time
}

// Price returns the on-demand Linux rate (USD per hour) for an instance type in a region. ok is
// false when none can be found; indicative is true when it comes from the built-in table
// because the Pricing API did not answer.
func (s *Service) Price(ctx context.Context, region, itype string) (usd float64, ok, indicative bool) {
	key := region + "|" + itype
	s.mu.Lock()
	if s.prices == nil {
		s.prices = map[string]priceEntry{}
	}
	e, hit := s.prices[key]
	s.mu.Unlock()
	if hit && s.now().Sub(e.at) < 24*time.Hour {
		return e.usd, e.ok, e.indicative
	}
	usd, err := s.lookup(ctx, region, itype)
	switch {
	case err == nil && usd > 0:
		e = priceEntry{usd: usd, ok: true, at: s.now()}
	default:
		if p, found := indicativePrice[itype]; found && region == "us-east-1" {
			e = priceEntry{usd: p, ok: true, indicative: true, at: s.now().Add(-23 * time.Hour)} // retry the API within the hour
		} else {
			e = priceEntry{at: s.now().Add(-23 * time.Hour)}
		}
	}
	s.mu.Lock()
	s.prices[key] = e
	s.mu.Unlock()
	return e.usd, e.ok, e.indicative
}

func (s *Service) lookup(ctx context.Context, region, itype string) (float64, error) {
	if s.Pricing == nil {
		return 0, fmt.Errorf("pricing is not configured")
	}
	eq := func(field, value string) pricingtypes.Filter {
		return pricingtypes.Filter{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String(field), Value: aws.String(value)}
	}
	out, err := s.Pricing.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{eq("instanceType", itype), eq("regionCode", region), eq("operatingSystem", "Linux"),
			eq("tenancy", "Shared"), eq("preInstalledSw", "NA"), eq("capacitystatus", "Used")},
		MaxResults: aws.Int32(5),
	})
	if err != nil {
		return 0, err
	}
	for _, raw := range out.PriceList {
		if v := parseOnDemand(raw); v > 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no on-demand price for %s in %s", itype, region)
}

// parseOnDemand pulls the USD-per-hour rate out of one Pricing API product document.
func parseOnDemand(raw string) float64 {
	var doc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string            `json:"unit"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return 0
	}
	for _, t := range doc.Terms.OnDemand {
		for _, d := range t.PriceDimensions {
			if d.Unit != "Hrs" {
				continue
			}
			if v, err := strconv.ParseFloat(d.PricePerUnit["USD"], 64); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

// indicativePrice is the fallback table (us-east-1 on-demand Linux, USD per hour). It is only
// shown, flagged as indicative, when the Pricing API cannot be reached.
var indicativePrice = map[string]float64{
	"t4g.medium": 0.0336, "t4g.large": 0.0672, "m7g.large": 0.0816, "m7g.xlarge": 0.1632, "c7g.xlarge": 0.1450,
	"r7g.large": 0.1071, "im4gn.large": 0.1819, "g5g.xlarge": 0.4200,
	"m7i.large": 0.1008, "m7i.xlarge": 0.2016, "c7i.xlarge": 0.1785, "g4dn.xlarge": 0.5260,
}

// --- catalogue ----------------------------------------------------------------------------------

// CatalogueType is one row of the template editor's instance list.
type CatalogueType struct {
	Type       string  `json:"type"`
	Arch       string  `json:"arch"` // arm64 | x86_64
	VCPU       int     `json:"vcpu"`
	MemoryMiB  int64   `json:"memory_mib"`
	Extra      string  `json:"extra"` // "EBS only", "937 GB local NVMe", "NVIDIA T4G GPU"
	HourlyUSD  float64 `json:"hourly_usd"`
	PriceKnown bool    `json:"price_known"`
	Indicative bool    `json:"indicative,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// curated is the list the editor offers, with a one-line note each.
var curated = []struct{ Type, Note string }{
	{"t4g.medium", "burstable"}, {"t4g.large", "burstable: slows under sustained load"}, {"m7g.large", "default pick"},
	{"c7g.xlarge", "compute-heavy builds"}, {"m7g.xlarge", "room for bigger repos"}, {"r7g.large", "memory-heavy"},
	{"im4gn.large", "about 2x the price: only if you need local disk"}, {"g5g.xlarge", "GPU: Android with graphics, ML"},
	{"m7i.large", "x86 default pick"}, {"c7i.xlarge", "compute-heavy builds"}, {"m7i.xlarge", ""},
	{"g4dn.xlarge", "GPU: x86 Android emulators"},
}

// Catalogue returns the curated types with specs from EC2 and prices from the Pricing API (the
// controller's own region), cheapest first within each architecture.
func (s *Service) Catalogue(ctx context.Context) ([]CatalogueType, error) {
	names := make([]string, len(curated))
	for i, c := range curated {
		names[i] = c.Type
	}
	ids := make([]types.InstanceType, len(names))
	for i, n := range names {
		ids[i] = types.InstanceType(n)
	}
	out, err := s.EC2(s.Home).DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: ids})
	if err != nil {
		return nil, fmt.Errorf("DescribeInstanceTypes: %w", err)
	}
	info := map[string]types.InstanceTypeInfo{}
	for _, it := range out.InstanceTypes {
		info[string(it.InstanceType)] = it
	}
	var res []CatalogueType
	for _, c := range curated {
		it, ok := info[c.Type]
		if !ok {
			continue // not offered in this region
		}
		ct := CatalogueType{Type: c.Type, Note: c.Note, Arch: "x86_64", Extra: "EBS only"}
		if it.VCpuInfo != nil {
			ct.VCPU = int(aws.ToInt32(it.VCpuInfo.DefaultVCpus))
		}
		if it.MemoryInfo != nil {
			ct.MemoryMiB = aws.ToInt64(it.MemoryInfo.SizeInMiB)
		}
		if it.ProcessorInfo != nil {
			for _, a := range it.ProcessorInfo.SupportedArchitectures {
				if a == types.ArchitectureTypeArm64 {
					ct.Arch = "arm64"
				}
			}
		}
		if it.InstanceStorageInfo != nil && it.InstanceStorageInfo.TotalSizeInGB != nil {
			ct.Extra = fmt.Sprintf("%d GB local NVMe", aws.ToInt64(it.InstanceStorageInfo.TotalSizeInGB))
		}
		if it.GpuInfo != nil && len(it.GpuInfo.Gpus) > 0 {
			g := it.GpuInfo.Gpus[0]
			ct.Extra = strings.TrimSpace(aws.ToString(g.Manufacturer) + " " + aws.ToString(g.Name) + " GPU")
		}
		ct.HourlyUSD, ct.PriceKnown, ct.Indicative = s.Price(ctx, s.Home, c.Type)
		res = append(res, ct)
	}
	sort.SliceStable(res, func(i, j int) bool {
		if res[i].Arch != res[j].Arch {
			return res[i].Arch < res[j].Arch
		}
		return res[i].HourlyUSD < res[j].HourlyUSD
	})
	return res, nil
}
