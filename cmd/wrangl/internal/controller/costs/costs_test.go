package costs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

const m7gDoc = `{"product":{},"terms":{"OnDemand":{"X.JRTCKXETXF":{"priceDimensions":{"X.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.0816000000"}}}}}}}`

type fakePricing struct{ doc map[string]string }

func (f fakePricing) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	for _, flt := range in.Filters {
		if aws.ToString(flt.Field) == "instanceType" {
			if d, ok := f.doc[aws.ToString(flt.Value)]; ok {
				return &pricing.GetProductsOutput{PriceList: []string{d}}, nil
			}
		}
	}
	return &pricing.GetProductsOutput{}, nil
}

type fakeCost struct {
	mtd, rest string
	err       error
}

func (f fakeCost) GetCostAndUsage(context.Context, *costexplorer.GetCostAndUsageInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &costexplorer.GetCostAndUsageOutput{ResultsByTime: []cetypes.ResultByTime{{Total: map[string]cetypes.MetricValue{"UnblendedCost": {Amount: aws.String(f.mtd)}}}}}, nil
}

func (f fakeCost) GetCostForecast(context.Context, *costexplorer.GetCostForecastInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostForecastOutput, error) {
	return &costexplorer.GetCostForecastOutput{Total: &cetypes.MetricValue{Amount: aws.String(f.rest)}}, nil
}

type fakeEC2 struct {
	region string
	by     map[string][]types.Instance
}

func (f fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: f.by[f.region]}}}, nil
}

func (f fakeEC2) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{}, nil
}

func inst(id, typ, state, name string, tags ...string) types.Instance {
	i := types.Instance{InstanceId: aws.String(id), InstanceType: types.InstanceType(typ), State: &types.InstanceState{Name: types.InstanceStateName(state)},
		Architecture: types.ArchitectureValuesArm64, LaunchTime: aws.Time(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)), VpcId: aws.String("vpc-1"), PrivateIpAddress: aws.String("10.0.0.5"),
		Placement: &types.Placement{AvailabilityZone: aws.String("us-east-1a")}}
	i.Tags = []types.Tag{{Key: aws.String("Name"), Value: aws.String(name)}}
	for n := 0; n+1 < len(tags); n += 2 {
		i.Tags = append(i.Tags, types.Tag{Key: aws.String(tags[n]), Value: aws.String(tags[n+1])})
	}
	return i
}

func svc(c fakeCost) *Service {
	by := map[string][]types.Instance{
		"us-east-1": {inst("i-run", "m7g.large", "running", "runner-1", "wrangl:managed", "true"), inst("i-stopped", "m7g.large", "stopped", "old")},
		"eu-west-1": {inst("i-eu", "c7g.xlarge", "running", "eu box"), inst("i-odd", "z9.weird", "running", "odd")},
	}
	return &Service{
		Pricing: fakePricing{doc: map[string]string{"m7g.large": m7gDoc}},
		Cost:    c,
		EC2:     func(r string) EC2API { return fakeEC2{region: r, by: by} },
		Regions: func(context.Context) ([]string, error) { return []string{"us-east-1", "eu-west-1"}, nil },
		Home:    "us-east-1", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) },
	}
}

func TestParseOnDemand(t *testing.T) {
	if v := parseOnDemand(m7gDoc); v != 0.0816 {
		t.Fatalf("got %v", v)
	}
	if parseOnDemand(`{"terms":{}}`) != 0 || parseOnDemand(`not json`) != 0 {
		t.Fatal("garbage must parse to 0")
	}
}

func TestRefreshSummaryAndInventory(t *testing.T) {
	s := svc(fakeCost{mtd: "12.50", rest: "30.25"})
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := s.Cached()
	sum := snap.Summary
	if sum.Running != 3 {
		t.Fatalf("running %d", sum.Running)
	}
	// m7g.large is priced by the API (0.0816); c7g.xlarge only by the indicative table, and only
	// in us-east-1, so the eu-west-1 one is unpriced, as is the made-up type.
	if sum.HourlyUSD != 0.0816 || sum.UnpricedRunning != 2 {
		t.Fatalf("hourly %v unpriced %d", sum.HourlyUSD, sum.UnpricedRunning)
	}
	if !sum.CostAvailable || sum.MonthToDateUSD != 12.5 || sum.ForecastRestUSD != 30.25 || sum.ProjectedUSD != 42.75 {
		t.Fatalf("bill %+v", sum)
	}
	if len(snap.Instances) != 4 {
		t.Fatalf("instances %d", len(snap.Instances))
	}
	if snap.Instances[0].State != "running" {
		t.Fatal("running instances sort first")
	}
	var managed, stopped *Instance
	for i := range snap.Instances {
		switch snap.Instances[i].ID {
		case "i-run":
			managed = &snap.Instances[i]
		case "i-stopped":
			stopped = &snap.Instances[i]
		}
	}
	if !managed.Managed || managed.Name != "runner-1" || managed.Arch != "arm64" {
		t.Fatalf("managed: %+v", managed)
	}
	if stopped.HourlyUSD != 0 || !stopped.PriceKnown {
		t.Fatalf("a stopped instance costs no compute: %+v", stopped)
	}
}

func TestCostFailureStillShowsInstances(t *testing.T) {
	s := svc(fakeCost{err: errors.New("AccessDenied: ce:GetCostAndUsage")})
	s.Refresh(context.Background())
	snap := s.Cached()
	if snap.Summary.CostAvailable || snap.Summary.CostError == "" {
		t.Fatalf("summary %+v", snap.Summary)
	}
	if snap.Summary.HourlyUSD == 0 || len(snap.Instances) == 0 {
		t.Fatal("the hourly rate and the instance list do not depend on Cost Explorer")
	}
}

func TestIndicativeFallbackIsFlagged(t *testing.T) {
	s := svc(fakeCost{})
	s.Pricing = fakePricing{} // knows nothing
	usd, ok, ind := s.Price(context.Background(), "us-east-1", "m7g.large")
	if !ok || !ind || usd != 0.0816 {
		t.Fatalf("%v %v %v", usd, ok, ind)
	}
	if _, ok, _ := s.Price(context.Background(), "ap-south-1", "m7g.large"); ok {
		t.Fatal("the indicative table is us-east-1 only")
	}
}
