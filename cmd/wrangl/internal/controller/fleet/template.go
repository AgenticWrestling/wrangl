package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
)

// DefaultIdleTimeout is how long a controller-launched runner may sit idle before the
// reaper terminates it.
const DefaultIdleTimeout = 30 * time.Minute

// Default AMIs (public SSM parameters for Amazon Linux 2023) by EC2 arch name.
var defaultAMI = map[string]string{
	"arm64":  "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64",
	"x86_64": "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64",
}

// TemplateFile is the YAML accepted by `wrangl template create -f` (spec §8).
type TemplateFile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Arch        string `yaml:"arch"`
	Min         struct {
		VCPU int    `yaml:"vcpu"`
		RAM  string `yaml:"ram"`
		Disk string `yaml:"disk"`
	} `yaml:"min"`
	InstanceTypes []string `yaml:"instance_types"`
	AMI           string   `yaml:"ami"`
	MaxRunners    *int     `yaml:"max_runners"`
	IdleTimeout   string   `yaml:"idle_timeout"`
	MiseTOML      string   `yaml:"mise_toml"`
	SetupScript   string   `yaml:"setup_script"`
	Agent         any      `yaml:"agent"`
}

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ParseTemplate validates a template file and converts it to insert parameters.
func ParseTemplate(b []byte) (dbgen.CreateTemplateParams, error) {
	var f TemplateFile
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		return dbgen.CreateTemplateParams{}, fmt.Errorf("template: %w", err)
	}
	var p dbgen.CreateTemplateParams
	if !templateName.MatchString(f.Name) {
		return p, fmt.Errorf("template name %q: lowercase letters, digits and '-', max 40", f.Name)
	}
	if f.Arch == "" {
		f.Arch = "arm64"
	}
	if f.Arch != "arm64" && f.Arch != "x86_64" {
		return p, fmt.Errorf("arch %q: want arm64 or x86_64", f.Arch)
	}
	if f.Min.VCPU <= 0 {
		return p, fmt.Errorf("min.vcpu must be > 0")
	}
	ram, err := parseSize(f.Min.RAM, "MiB")
	if err != nil {
		return p, fmt.Errorf("min.ram: %w", err)
	}
	disk, err := parseSize(f.Min.Disk, "GiB")
	if err != nil {
		return p, fmt.Errorf("min.disk: %w", err)
	}
	idle := DefaultIdleTimeout
	if f.IdleTimeout != "" {
		if idle, err = time.ParseDuration(f.IdleTimeout); err != nil || idle < time.Minute {
			return p, fmt.Errorf("idle_timeout %q: want a duration of at least 1m", f.IdleTimeout)
		}
	}
	maxRunners := 1
	if f.MaxRunners != nil {
		maxRunners = *f.MaxRunners
	}
	if maxRunners < 0 {
		return p, fmt.Errorf("max_runners must be >= 0")
	}
	if f.AMI == "" {
		f.AMI = defaultAMI[f.Arch]
	}
	if f.InstanceTypes == nil {
		f.InstanceTypes = []string{}
	}
	it, _ := json.Marshal(f.InstanceTypes)
	agent, err := json.Marshal(normalize(f.Agent))
	if err != nil {
		return p, fmt.Errorf("agent: %w", err)
	}
	return dbgen.CreateTemplateParams{
		Name: f.Name, Description: f.Description, MiseToml: f.MiseTOML, SetupScript: f.SetupScript,
		AgentJson: string(agent), MinVcpu: int64(f.Min.VCPU), MinRamMib: ram, MinDiskGib: disk,
		Arch: f.Arch, InstanceTypes: string(it), Ami: f.AMI, MaxRunners: int64(maxRunners),
		IdleTimeoutS: int64(idle / time.Second),
	}, nil
}

// parseSize parses "16GiB", "512MiB", "100G" or a bare number (in unit) into unit.
func parseSize(s, unit string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("required")
	}
	mult := map[string]float64{"MiB": 1, "M": 1, "GiB": 1024, "G": 1024, "TiB": 1024 * 1024, "T": 1024 * 1024}
	num, suf := s, ""
	for i, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			num, suf = s[:i], strings.TrimSpace(s[i:])
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
	}
	mib := v
	if suf == "" {
		mib = v * mult[unit]
	} else if m, ok := mult[suf]; ok {
		mib = v * m
	} else {
		return 0, fmt.Errorf("%q: unknown unit %q", s, suf)
	}
	return int64(mib / mult[unit]), nil
}

// normalize converts yaml.v3 map[string]any trees so encoding/json can marshal them.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
		return t
	case nil:
		return map[string]any{}
	}
	return v
}
