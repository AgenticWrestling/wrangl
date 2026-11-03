package api_test

import (
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/costs"
)

func TestAdoptInfoReachability(t *testing.T) {
	snap := costs.Snapshot{Instances: []costs.Instance{{ID: "i-ctl", Region: "us-east-1", VPC: "vpc-a", PrivateIP: "172.31.12.108"}}}
	same := costs.Instance{ID: "i-1", Region: "us-east-1", VPC: "vpc-a"}
	if ok, note := api.AdoptInfo(snap, "http://172.31.12.108:4223", same); !ok || note != "" {
		t.Fatalf("same VPC: %v %q", ok, note)
	}
	other := costs.Instance{ID: "i-2", Region: "eu-west-1", VPC: "vpc-b"}
	if ok, note := api.AdoptInfo(snap, "http://172.31.12.108:4223", other); ok || !strings.Contains(note, "VPC peering") {
		t.Fatalf("other region must warn about routing: %v %q", ok, note)
	}
	if ok, _ := api.AdoptInfo(snap, "", same); ok {
		t.Fatal("no enrol URL, no promise")
	}
}

func TestOtherInstancesSkipsRunners(t *testing.T) {
	snap := costs.Snapshot{Instances: []costs.Instance{{ID: "i-runner"}, {ID: "i-other"}}}
	got := api.OtherInstances(snap, map[string]bool{"i-runner": true})
	if len(got) != 1 || got[0].ID != "i-other" {
		t.Fatalf("%+v", got)
	}
}
