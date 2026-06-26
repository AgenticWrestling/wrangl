package tunnel

import (
	"fmt"
	"strconv"
	"strings"
)

type PortSpec struct {
	RemotePort int
	LocalPort  int
}

// ParsePortSpec parses "3000" or "3000:8080" (remote:local).
func ParsePortSpec(s string) (PortSpec, error) {
	if s == "" {
		return PortSpec{}, fmt.Errorf("port spec must not be empty")
	}
	parts := strings.SplitN(s, ":", 2)
	remote, err := strconv.Atoi(parts[0])
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid remote port %q: %w", parts[0], err)
	}
	if len(parts) == 1 {
		return PortSpec{RemotePort: remote, LocalPort: remote}, nil
	}
	local, err := strconv.Atoi(parts[1])
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid local port %q: %w", parts[1], err)
	}
	return PortSpec{RemotePort: remote, LocalPort: local}, nil
}
