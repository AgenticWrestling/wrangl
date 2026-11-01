// Package proto holds the wire types shared by controller and runner.
package proto

import "time"

// EnrollPath is the enrol listener endpoint (spec §7).
const EnrollPath = "/enroll"

// BinaryPath returns the download path for a runner binary; arch is `uname -m`.
func BinaryPath(arch string) string { return "/dl/wrangl-linux-" + arch }

// Facts are what the runner measured about its own machine.
type Facts struct {
	Arch    string `json:"arch"` // uname -m: aarch64 | x86_64
	VCPU    int    `json:"vcpu"`
	RAMMiB  int    `json:"ram_mib"`
	DiskGiB int    `json:"disk_gib"`
}

// EnrollRequest is POSTed by `wrangl runner bootstrap`.
type EnrollRequest struct {
	Token      string `json:"token"`
	NKeyPublic string `json:"nkey_public"`
	InstanceID string `json:"instance_id,omitempty"` // from IMDSv2; empty off EC2
	Facts      Facts  `json:"facts"`
}

// EnrollResponse carries everything the runner needs to connect.
type EnrollResponse struct {
	RunnerID int64    `json:"runner_id"`
	Name     string   `json:"name"`
	UserJWT  string   `json:"user_jwt"`
	NATSURLs []string `json:"nats_urls"`
	NATSCA   string   `json:"nats_ca"` // PEM; the runner pins it
}

// Heartbeat is published on wr.r.<id>.hb every HeartbeatInterval.
type Heartbeat struct {
	Time        time.Time `json:"time"`
	Version     string    `json:"version"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemPercent  float64   `json:"mem_percent"`
	DiskPercent float64   `json:"disk_percent"`
	Load1       float64   `json:"load1"`
	UptimeS     uint64    `json:"uptime_s"`
	// TemplateVersion is the template version last applied successfully (0 = none).
	TemplateVersion int64 `json:"template_version"`
	// Applying is true while a template apply is running.
	Applying bool `json:"applying"`
	// RunID is the run in progress (0 = none).
	RunID int64 `json:"run_id,omitempty"`
	// BinarySHA is the sha256 of the wrangl binary the daemon runs ("" = self-upgrade off).
	BinarySHA string `json:"binary_sha,omitempty"`
	// Upgrading is true while a self-upgrade is downloading or restarting.
	Upgrading bool `json:"upgrading,omitempty"`
}

// Upgrade is the cmd.upgrade request: replace the wrangl binary with the one the
// controller serves at <enrol URL>/dl/wrangl-linux-<arch>, which must hash to SHA256.
// The URL is the runner's own enrolment URL, never taken from the message.
type Upgrade struct {
	SHA256 string `json:"sha256"`
}

// TemplateApply is the cmd.template request: everything needed to (re)apply a template.
type TemplateApply struct {
	Version     int64  `json:"version"`
	MiseTOML    string `json:"mise_toml"`
	SetupScript string `json:"setup_script"`
}

// TemplateAck is the immediate reply to cmd.template; the outcome arrives as an event.
type TemplateAck struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// TemplateResult is published (JetStream) on wr.evt.<r>.template when an apply ends.
type TemplateResult struct {
	Version int64  `json:"version"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Log     string `json:"log"` // tail of the combined output
}

// Timing shared by both sides.
const (
	HeartbeatInterval = 10 * time.Second
	OfflineAfter      = 60 * time.Second // controller marks a runner offline
	// OrphanPoweroff: a runner that cannot reach its controller for this long powers off.
	// Instances launch with InstanceInitiatedShutdownBehavior=terminate, so that
	// terminates it: the safety net when the controller is gone and nothing reaps.
	OrphanPoweroff = 2 * time.Hour
)

// Ping is the reply to cmd.ping.
type Ping struct {
	RunnerID int64  `json:"runner_id"`
	Version  string `json:"version"`
}
