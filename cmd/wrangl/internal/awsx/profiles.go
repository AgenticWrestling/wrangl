package awsx

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/ini.v1"
)

// ListProfiles returns the profile names in $AWS_CONFIG_FILE or ~/.aws/config.
func ListProfiles() ([]string, error) {
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".aws", "config")
	}

	cfg, err := ini.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // credentials may come from the environment
		}
		return nil, err
	}

	var profiles []string
	for _, section := range cfg.Sections() {
		name := section.Name()
		if name == "DEFAULT" {
			continue
		}
		if name == "default" {
			profiles = append(profiles, "default")
			continue
		}
		if rest, ok := strings.CutPrefix(name, "profile "); ok {
			profiles = append(profiles, rest)
		}
	}
	return profiles, nil
}
