package menu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

func TestParseStringsAndObjects(t *testing.T) {
	var f struct {
		T []Entry `yaml:"instance_types"`
	}
	src := "instance_types:\n  - t4g.medium\n  - { type: c7g.xlarge, price: 0.145 }\n"
	if err := yaml.Unmarshal([]byte(src), &f); err != nil {
		t.Fatal(err)
	}
	if len(f.T) != 2 || f.T[0].Type != "t4g.medium" || f.T[0].Price != nil || f.T[1].Type != "c7g.xlarge" || *f.T[1].Price != 0.145 {
		t.Fatalf("%+v", f.T)
	}
	b, _ := json.Marshal(f.T)
	if string(b) != `["t4g.medium",{"type":"c7g.xlarge","price":0.145}]` {
		t.Fatalf("stored form: %s", b)
	}
	if got := Names(string(b)); strings.Join(got, ",") != "t4g.medium,c7g.xlarge" {
		t.Fatalf("names %v", got)
	}
	if got := Names(`["a","b"]`); strings.Join(got, ",") != "a,b" {
		t.Fatalf("legacy column: %v", got)
	}
	y, _ := yaml.Marshal(map[string]any{"instance_types": f.T})
	if !strings.Contains(string(y), "- t4g.medium") || !strings.Contains(string(y), "price: 0.145") {
		t.Fatalf("yaml: %s", y)
	}
	for _, bad := range []string{"instance_types: [{ type: x, price: -1 }]", "instance_types: [{ price: 1 }]"} {
		if yaml.Unmarshal([]byte(bad), &struct {
			T []Entry `yaml:"instance_types"`
		}{}) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

type fakeEC2 struct{}

func (fakeEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	info := map[string]ec2types.InstanceTypeInfo{
		"t4g.medium": {InstanceType: "t4g.medium", VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(2)}, MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(4096)},
			ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{"arm64"}}},
		"m7g.xlarge": {InstanceType: "m7g.xlarge", VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(4)}, MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(16384)},
			ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{"arm64"}}},
		"c7i.large": {InstanceType: "c7i.large", VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(2)}, MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(4096)},
			ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{"x86_64"}}},
	}
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, t := range in.InstanceTypes {
		if i, ok := info[string(t)]; ok {
			out.InstanceTypes = append(out.InstanceTypes, i)
		}
	}
	return out, nil
}

type fakePrices map[string]struct {
	usd        float64
	indicative bool
}

func (p fakePrices) Price(_ context.Context, _ string, t string) (float64, bool, bool) {
	v, ok := p[t]
	return v.usd, ok, v.indicative
}

func TestResolveCapacityAndPricePrecedence(t *testing.T) {
	half := 0.5
	l := Lookup{EC2: fakeEC2{}, Region: "us-east-1", Prices: fakePrices{"t4g.medium": {0.0336, false}, "m7g.xlarge": {0.163, true}}}
	es := []Entry{{Type: "t4g.medium"}, {Type: "m7g.xlarge", Price: &half}, {Type: "nope.large"}, {Type: "c7i.large"}}
	rs, warns, err := l.Resolve(context.Background(), "arm64", es, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || len(warns) != 2 {
		t.Fatalf("resolved %+v warnings %v", rs, warns)
	}
	a, b := rs[0], rs[1]
	if a.Type != "t4g.medium" || a.VCPU != 2 || a.RAMMiB != 3072 || a.HourlyUSD != 0.0336 || a.PriceSource != FromLive || !a.Launchable() {
		t.Fatalf("%+v", a)
	}
	if b.PriceSource != FromTemplate || b.HourlyUSD != 0.5 || b.RAMMiB != 15360 {
		t.Fatalf("explicit price must win over the lookup: %+v", b)
	}
	// Indicative fallback is flagged but launchable.
	rs, _, _ = Lookup{EC2: fakeEC2{}, Prices: fakePrices{"m7g.xlarge": {0.163, true}}}.Resolve(context.Background(), "arm64", []Entry{{Type: "m7g.xlarge"}}, 0)
	if rs[0].PriceSource != Indicative || !rs[0].Launchable() {
		t.Fatalf("%+v", rs[0])
	}
	// Unknown price: fail closed.
	rs, warns, _ = Lookup{EC2: fakeEC2{}, Prices: fakePrices{}}.Resolve(context.Background(), "arm64", []Entry{{Type: "t4g.medium"}}, 0)
	if rs[0].Launchable() || rs[0].PriceSource != Unknown || len(warns) != 1 {
		t.Fatalf("%+v %v", rs[0], warns)
	}
	// ... unless the template gives one.
	rs, _, _ = Lookup{EC2: fakeEC2{}}.Resolve(context.Background(), "arm64", []Entry{{Type: "t4g.medium", Price: &half}}, 0)
	if !rs[0].Launchable() {
		t.Fatalf("%+v", rs[0])
	}
}

func TestVolumeAndEffectivePrice(t *testing.T) {
	if g, ok := VolumeGiB(30, 10, 8, 0); g != 30 || !ok {
		t.Fatalf("%d %v", g, ok)
	}
	if g, ok := VolumeGiB(30, 200, 8, 0); g != 208 || !ok {
		t.Fatalf("a big task gets a bigger volume: %d %v", g, ok)
	}
	if _, ok := VolumeGiB(30, 600, 8, 0); ok {
		t.Fatal("over the sanity limit")
	}
	if _, ok := VolumeGiB(30, 600, 8, 1000); !ok {
		t.Fatal("explicit max_disk")
	}
	r := Resolved{HourlyUSD: 0.1}
	if p := r.EffectivePrice(100); p < 0.1109 || p > 0.1111 {
		t.Fatalf("%f", p)
	}
}
