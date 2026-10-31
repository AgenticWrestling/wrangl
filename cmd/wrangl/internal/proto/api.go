package proto

import (
	"encoding/json"
	"time"
)

// RunnerView is a runner as the API returns it.
type RunnerView struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	State           string          `json:"state"`
	Template        string          `json:"template"`
	Launched        bool            `json:"launched"` // false = adopted via enroll-token
	InstanceID      string          `json:"instance_id,omitempty"`
	InstanceType    string          `json:"instance_type,omitempty"`
	AZ              string          `json:"az,omitempty"`
	PrivateIP       string          `json:"private_ip,omitempty"`
	Arch            string          `json:"arch,omitempty"`
	VCPU            int64           `json:"vcpu,omitempty"`
	RAMMiB          int64           `json:"ram_mib,omitempty"`
	DiskGiB         int64           `json:"disk_gib,omitempty"`
	LastHeartbeatAt string          `json:"last_heartbeat_at,omitempty"`
	IdleSince       string          `json:"idle_since,omitempty"`
	IdleTimeoutS    int64           `json:"idle_timeout_s,omitempty"`
	LaunchedAt      string          `json:"launched_at,omitempty"`
	EnrolledAt      string          `json:"enrolled_at,omitempty"`
	TerminatedAt    string          `json:"terminated_at,omitempty"`
	Metrics         json.RawMessage `json:"metrics,omitempty"`
	Cordoned        bool            `json:"cordoned"`
	Draining        bool            `json:"draining"`
	TemplateVersion int64           `json:"template_version"` // applied on the runner
	WantVersion     int64           `json:"want_version,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	TemplateLog     string          `json:"template_log,omitempty"`
}

// TemplateView is a runner template as the API returns it.
type TemplateView struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Arch          string          `json:"arch"`
	MinVCPU       int64           `json:"min_vcpu"`
	MinRAMMiB     int64           `json:"min_ram_mib"`
	MinDiskGiB    int64           `json:"min_disk_gib"`
	InstanceTypes []string        `json:"instance_types"`
	AMI           string          `json:"ami"`
	MaxRunners    int64           `json:"max_runners"`
	IdleTimeoutS  int64           `json:"idle_timeout_s"`
	Version       int64           `json:"version"`
	FailedVersion int64           `json:"failed_version,omitempty"`
	MiseTOML      string          `json:"mise_toml"`
	SetupScript   string          `json:"setup_script,omitempty"`
	Agent         json.RawMessage `json:"agent"`
}

// LaunchRequest is POST /api/v1/runners.
type LaunchRequest struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
}

// EnrollTokenRequest is POST /api/v1/enroll-tokens.
type EnrollTokenRequest struct {
	Template string `json:"template"`
	TTLS     int64  `json:"ttl_s"`
}

// EnrollTokenResponse carries the token and the command to run on the box.
type EnrollTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	EnrollURL string    `json:"enroll_url"`
	Command   string    `json:"command"`
}

// APIError is the body of every non-2xx response.
type APIError struct {
	Error string `json:"error"`
}
