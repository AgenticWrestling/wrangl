package awsx

import (
	"bytes"
	"context"
	"testing"
)

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]Target{
		"":                        {},
		"us-west-2":               {Region: "us-west-2"},
		"us-west-2/dev":           {Region: "us-west-2", Name: "dev"},
		"us-west-2/bob@dev":       {Region: "us-west-2", User: "bob", Name: "dev"},
		"bob@dev":                 {User: "bob", Name: "dev"},
		"dev":                     {Name: "dev"},
		"i-0abc123def456":         {ID: "i-0abc123def456", Name: "i-0abc123def456"},
		"eu-west-1/i-0abc123def4": {Region: "eu-west-1", ID: "i-0abc123def4", Name: "i-0abc123def4"},
	} {
		if got := ParseTarget(in); got != want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func testResolver(t *testing.T) (*Resolver, *Session, *string) {
	s, _ := newTestSession()
	var picked string
	r := &Resolver{
		Profile: "dev", Out: &bytes.Buffer{},
		NewSession: func(_ context.Context, p, region string) (*Session, error) {
			s.Profile = p
			picked = region
			return s, nil
		},
		EnsureSSO:    func(string) error { return nil },
		RecordUsage:  func(string) error { return nil },
		PickInstance: func(l []Instance) (*Instance, error) { return &l[0], nil },
	}
	return r, s, &picked
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	r, _, region := testResolver(t)
	t.Setenv("AWS_REGION", "ap-south-1")
	_, i, tg, err := r.Resolve(ctx, "bob@dev")
	if err != nil || i.ID != "i-0aaa111111" || tg.User != "bob" || *region != "ap-south-1" {
		t.Fatalf("%+v %+v %v region=%q", i, tg, err, *region)
	}
	if _, i, _, err = r.Resolve(ctx, "us-east-1/i-0bbb222222"); err != nil || i.Name != "old" || *region != "us-east-1" {
		t.Fatalf("%+v %v region=%q", i, err, *region)
	}
	// no match: falls back to the picker (first running instance)
	if _, i, _, err = r.Resolve(ctx, "nosuch"); err != nil || i.Name != "dev" {
		t.Fatalf("%+v %v", i, err)
	}
	// stopped states for `start`
	if _, i, _, err = r.Resolve(ctx, "", "stopped"); err != nil || i.Name != "old" {
		t.Fatalf("%+v %v", i, err)
	}
	r.Region = "us-west-1" // flag wins over the argument
	if _, _, _, err = r.Resolve(ctx, "us-east-2/dev"); err != nil || *region != "us-west-1" {
		t.Fatalf("%v region=%q", err, *region)
	}
}
