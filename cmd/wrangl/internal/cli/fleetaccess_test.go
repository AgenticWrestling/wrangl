package cli

import "testing"

func TestRegionFromAZ(t *testing.T) {
	for in, want := range map[string]string{"us-west-2b": "us-west-2", "eu-central-1a": "eu-central-1", "": "", "us-east-1": "us-east-1"} {
		if got := regionFromAZ(in); got != want {
			t.Errorf("regionFromAZ(%q) = %q, want %q", in, got, want)
		}
	}
}
