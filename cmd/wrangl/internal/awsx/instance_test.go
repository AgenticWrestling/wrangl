package awsx

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"testing"
)

func newTestSession() (*Session, *fakeEC2) {
	f := &fakeEC2{insts: []types.Instance{inst("i-0aaa111111", "dev", "running"), inst("i-0bbb222222", "old", "stopped"), inst("i-0ccc333333", "dup", "running"), inst("i-0ddd444444", "dup", "running")}}
	return &Session{Region: "us-west-2", EC2: f}, f
}

func TestByName(t *testing.T) {
	s, _ := newTestSession()
	ctx := context.Background()
	i, err := s.ByName(ctx, "dev")
	if err != nil || i.ID != "i-0aaa111111" || i.Region != "us-west-2" || i.VPCID != "vpc-1" || len(i.SecurityGroupIDs) != 1 {
		t.Fatalf("got %+v %v", i, err)
	}
	if _, err := s.ByName(ctx, "old"); err == nil {
		t.Fatal("stopped instance must not match a running lookup")
	}
	if i, err := s.ByName(ctx, "old", "stopped"); err != nil || i.State != "stopped" {
		t.Fatalf("got %+v %v", i, err)
	}
	if _, err := s.ByName(ctx, "dup"); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestListStartStop(t *testing.T) {
	s, f := newTestSession()
	ctx := context.Background()
	l, _ := s.List(ctx, "stopped")
	if len(l) != 1 || l[0].Name != "old" {
		t.Fatalf("got %+v", l)
	}
	all, _ := s.List(ctx)
	if len(all) != 4 {
		t.Fatalf("got %d", len(all))
	}
	if err := s.Start(ctx, "i-0bbb222222"); err != nil || len(f.started) != 1 {
		t.Fatal(err, f.started)
	}
	if err := s.Stop(ctx, "i-0aaa111111"); err != nil || len(f.stopped) != 1 {
		t.Fatal(err, f.stopped)
	}
	if g, err := s.Get(ctx, "i-0aaa111111"); err != nil || g.Name != "dev" {
		t.Fatal(g, err)
	}
}

func TestShapes(t *testing.T) {
	for s, want := range map[string]bool{"us-west-2": true, "eu-central-1": true, "ap-southeast-2": true, "us-gov-west-1": true, "myhost": false, "my-host": false, "a-b-c": false} {
		if LooksLikeRegion(s) != want {
			t.Errorf("LooksLikeRegion(%q) != %v", s, want)
		}
	}
	if !IsInstanceID("i-0abc123def456") || IsInstanceID("i-xyz") || IsInstanceID("host") {
		t.Error("IsInstanceID")
	}
	if r, n, err := ParseRegionName("us-west-2/myhost"); err != nil || r != "us-west-2" || n != "myhost" {
		t.Error(r, n, err)
	}
	if _, _, err := ParseRegionName("myhost"); err == nil {
		t.Error("want error")
	}
}
