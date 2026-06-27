// cmd/ssm/internal/cwlogs/cwlogs_test.go
package cwlogs_test

import (
	"testing"

	"github.com/nreid/devhost/cmd/ssm/internal/cwlogs"
)

func TestLogStreamName(t *testing.T) {
	got := cwlogs.LogStreamName("cmd-123", "i-abc")
	want := "cmd-123/i-abc/aws-runshellscript/0.awsrunshellscriptplugin"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTail_integration(t *testing.T) {
	t.Skip("integration test — requires live AWS credentials and a running SSM command")
}
