package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
)

func TestFleetTeardown(t *testing.T) {
	f := provision.NewFake()
	ctx := context.Background()
	mine := &provision.Provisioner{EC2: f, SSM: f, ControllerID: "wr-mine"}
	other := &provision.Provisioner{EC2: f, SSM: f, ControllerID: "wr-other"}
	spec := provision.Spec{Runner: "a", Template: "go", Arch: "arm64", MinVCPU: 1, MinRAMMiB: 1, DiskGiB: 8, AMI: "ami-1"}
	a, _ := mine.Launch(ctx, spec)
	b, _ := other.Launch(ctx, spec)
	f.AddForeign("i-prod", map[string]string{"Name": "prod-db"})

	old := TeardownEC2
	TeardownEC2 = func(context.Context, string, string) (provision.EC2API, error) { return f, nil }
	defer func() { TeardownEC2 = old }()

	// Without --yes, anything but "yes" aborts and terminates nothing.
	out, err := run(t, "no\n", "fleet", "teardown", "--controller-id", "wr-mine")
	if err == nil || !strings.Contains(out, a.ID) || f.State(a.ID) == "terminated" {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	out, err = run(t, "yes\n", "fleet", "teardown", "--controller-id", "wr-mine")
	if err != nil || f.State(a.ID) != "terminated" || f.State(b.ID) == "terminated" || f.State("i-prod") != "running" {
		t.Fatalf("scoped teardown: %v\n%s", err, out)
	}
	out, err = run(t, "", "fleet", "teardown", "--all-controllers", "--yes")
	if err != nil || f.State(b.ID) != "terminated" || f.State("i-prod") != "running" {
		t.Fatalf("all-controllers teardown: %v\n%s", err, out)
	}
	if out, _ := run(t, "", "fleet", "teardown", "--all-controllers", "--yes"); !strings.Contains(out, "no wrangl instances") {
		t.Fatalf("empty teardown: %s", out)
	}
}
