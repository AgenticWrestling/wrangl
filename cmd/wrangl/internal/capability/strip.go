package capability

import "strings"

// WithoutAgent drops agent:* requirements. A run on a remote template runs its agent on a driver runner of
// another template; what the driver's template allows is not the run's concern.
func WithoutAgent(reqs []Requirement) []Requirement {
	out := make([]Requirement, 0, len(reqs))
	for _, r := range reqs {
		if !strings.HasPrefix(r.Key, "agent:") {
			out = append(out, r)
		}
	}
	return out
}
