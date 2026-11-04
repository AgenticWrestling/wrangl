package tunnel_test

import (
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/tunnel"
)

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		input      string
		wantRemote int
		wantLocal  int
		wantErr    bool
	}{
		{"3000", 3000, 3000, false},
		{"3000:8080", 3000, 8080, false},
		{"abc", 0, 0, true},
		{"3000:abc", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		spec, err := tunnel.ParsePortSpec(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePortSpec(%q): expected error, got nil", c.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortSpec(%q): unexpected error: %v", c.input, err)
			continue
		}
		if spec.RemotePort != c.wantRemote || spec.LocalPort != c.wantLocal {
			t.Errorf("ParsePortSpec(%q): got remote=%d local=%d, want remote=%d local=%d",
				c.input, spec.RemotePort, spec.LocalPort, c.wantRemote, c.wantLocal)
		}
	}
}
