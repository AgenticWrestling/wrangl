// Package menu is a template's instance menu (ADR 0009 section 7.5): the instance types a runner may be
// launched as, each with its capacity (what a session can use), its hourly price and where that price
// came from. An entry whose price is unknown is not launchable: a budget cannot be enforced against an
// unknown number (decision 8). Everything here is pure except Lookup, which asks EC2 and the pricing
// service.
package menu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

// Entry is one line of `instance_types`: a type name, or an object with an explicit price (USD per
// hour) that overrides the lookup.
type Entry struct {
	Type  string
	Price *float64
}

func (e Entry) validate() error {
	if strings.TrimSpace(e.Type) == "" {
		return fmt.Errorf("instance_types: an entry needs a type")
	}
	if e.Price != nil && *e.Price <= 0 {
		return fmt.Errorf("instance_types: price for %s must be > 0", e.Type)
	}
	return nil
}

// UnmarshalYAML accepts "t4g.medium" or { type: c7g.xlarge, price: 0.145 }.
func (e *Entry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.Type, e.Price = strings.TrimSpace(n.Value), nil
		return e.validate()
	}
	var o struct {
		Type  string   `yaml:"type"`
		Price *float64 `yaml:"price"`
	}
	d := n
	if err := d.Decode(&o); err != nil {
		return fmt.Errorf("instance_types entry: %w", err)
	}
	e.Type, e.Price = strings.TrimSpace(o.Type), o.Price
	return e.validate()
}

// MarshalYAML writes a plain entry as a string, a priced one as an object.
func (e Entry) MarshalYAML() (any, error) {
	if e.Price == nil {
		return e.Type, nil
	}
	return map[string]any{"type": e.Type, "price": *e.Price}, nil
}

// MarshalJSON stores a plain entry as a string (what the column always held) and a priced one as an object.
func (e Entry) MarshalJSON() ([]byte, error) {
	if e.Price == nil {
		return json.Marshal(e.Type)
	}
	return json.Marshal(struct {
		Type  string  `json:"type"`
		Price float64 `json:"price"`
	}{e.Type, *e.Price})
}

// UnmarshalJSON reads either form.
func (e *Entry) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		e.Type, e.Price = s, nil
		return nil
	}
	var o struct {
		Type  string   `json:"type"`
		Price *float64 `json:"price"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	e.Type, e.Price = o.Type, o.Price
	return nil
}

// Parse reads the stored column.
func Parse(raw string) []Entry {
	var es []Entry
	json.Unmarshal([]byte(raw), &es)
	return es
}

// Names are the entries' type names, in preference order (the legacy list).
func Names(raw string) []string {
	out := []string{}
	for _, e := range Parse(raw) {
		out = append(out, e.Type)
	}
	return out
}

// Source says where a price came from.
type Source string

const (
	FromTemplate Source = "template" // explicit price: in the template
	FromLive     Source = "live"     // costs.Service.Price from the pricing API
	Indicative   Source = "indicative"
	Unknown      Source = "unknown"
)

// Resolved is a menu entry with capacity and price.
type Resolved struct {
	Type          string
	VCPU          int64
	RAMMiB        int64 // usable by sessions: the instance's memory minus the reserve
	HourlyUSD     float64
	PriceSource   Source
	InstanceRAM   int64
	SupportsLocal bool
}

// Launchable is false for an entry with an unknown price (fail closed).
func (r Resolved) Launchable() bool { return r.PriceSource != Unknown }

// DiskUSDPerGiBHour is the EBS gp3 rate (about $0.08 per GiB-month) added to an entry's price per GiB of
// volume (ADR 0009 section 7.5; becomes a placement setting with the budget).
const DiskUSDPerGiBHour = 0.00011

// DefaultMaxDiskGiB is the sanity limit on a volume (the template's max_disk, default 500 GiB).
const DefaultMaxDiskGiB = 500

// EffectivePrice is the hourly price with a diskGiB volume.
func (r Resolved) EffectivePrice(diskGiB int64) float64 {
	return r.HourlyUSD + float64(diskGiB)*DiskUSDPerGiBHour
}

// VolumeGiB sizes a new runner's volume: the template minimum, or the hosted runs' disk needs plus the
// reserve when that is more. ok is false when it would exceed maxDisk (0 = DefaultMaxDiskGiB).
func VolumeGiB(minDisk, hostedDisk, reserveDisk, maxDisk int64) (gib int64, ok bool) {
	if maxDisk <= 0 {
		maxDisk = DefaultMaxDiskGiB
	}
	gib = max(minDisk, hostedDisk+reserveDisk)
	return gib, gib <= maxDisk
}

// EC2 is the part of the EC2 client the menu needs.
type EC2 interface {
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
}

// Pricer is costs.Service's price lookup.
type Pricer interface {
	Price(ctx context.Context, region, itype string) (usd float64, ok, indicative bool)
}

// Lookup resolves a menu against EC2 and the pricing service. Prices may be nil (every price is then
// unknown unless the template gives one).
type Lookup struct {
	EC2    EC2
	Prices Pricer
	Region string
}

// Resolve gives each entry its capacity and price, in menu order. Entries that do not exist in the
// region or have another architecture are dropped, each with a warning.
func (l Lookup) Resolve(ctx context.Context, arch string, entries []Entry, reserveRAMMiB int64) ([]Resolved, []string, error) {
	if len(entries) == 0 {
		return nil, nil, nil
	}
	var ts []ec2types.InstanceType
	for _, e := range entries {
		ts = append(ts, ec2types.InstanceType(e.Type))
	}
	out, err := l.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: ts})
	if err != nil {
		return nil, nil, fmt.Errorf("DescribeInstanceTypes: %w", err)
	}
	by := map[string]ec2types.InstanceTypeInfo{}
	for _, it := range out.InstanceTypes {
		by[string(it.InstanceType)] = it
	}
	var res []Resolved
	var warns []string
	for _, e := range entries {
		it, ok := by[e.Type]
		if !ok {
			warns = append(warns, fmt.Sprintf("instance type %s does not exist in this region; dropped from the menu", e.Type))
			continue
		}
		archOK := false
		for _, a := range it.ProcessorInfo.SupportedArchitectures {
			archOK = archOK || string(a) == arch
		}
		if !archOK {
			warns = append(warns, fmt.Sprintf("instance type %s is not %s; dropped from the menu", e.Type, arch))
			continue
		}
		mem := aws.ToInt64(it.MemoryInfo.SizeInMiB)
		r := Resolved{Type: e.Type, VCPU: int64(aws.ToInt32(it.VCpuInfo.DefaultVCpus)), InstanceRAM: mem,
			RAMMiB: max(mem-reserveRAMMiB, 0), SupportsLocal: aws.ToBool(it.InstanceStorageSupported), PriceSource: Unknown}
		switch {
		case e.Price != nil:
			r.HourlyUSD, r.PriceSource = *e.Price, FromTemplate
		case l.Prices != nil:
			if usd, ok, ind := l.Prices.Price(ctx, l.Region, e.Type); ok {
				r.HourlyUSD, r.PriceSource = usd, FromLive
				if ind {
					r.PriceSource = Indicative
				}
			}
		}
		if r.PriceSource == Unknown {
			warns = append(warns, fmt.Sprintf("no price for %s: it will not be launched until the template gives one (`{ type: %s, price: 0.123 }`)", e.Type, e.Type))
		}
		res = append(res, r)
	}
	return res, warns, nil
}
