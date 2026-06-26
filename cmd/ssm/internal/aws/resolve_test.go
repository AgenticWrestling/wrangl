// cmd/ssm/internal/aws/resolve_test.go
package aws_test

import (
	"context"
	"testing"

	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

// Integration guard — skip if no AWS credentials available
func TestResolveInstance_noMatch(t *testing.T) {
	t.Skip("integration test — run manually with valid AWS credentials")
	_, err := ssmaws.ResolveInstance(context.Background(), "default", "us-east-1", "nonexistent-host-xyz")
	if err == nil {
		t.Fatal("expected error for no match")
	}
}

func TestParseRegionName(t *testing.T) {
	region, name, err := ssmaws.ParseRegionName("us-west-2/myhost")
	if err != nil || region != "us-west-2" || name != "myhost" {
		t.Fatalf("got region=%q name=%q err=%v", region, name, err)
	}

	_, _, err = ssmaws.ParseRegionName("myhost")
	if err == nil {
		t.Fatal("expected error for missing region")
	}
}
