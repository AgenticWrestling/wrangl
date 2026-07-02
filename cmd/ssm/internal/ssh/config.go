// cmd/ssm/internal/ssh/config.go
package ssh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func UpsertHostBlock(name, instanceID, region, profile, osUser string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")

	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return err
	}

	var extra string
	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var rest string
	if err == nil {
		rest, extra = removeHostBlock(string(existing), name)
	}

	block := fmt.Sprintf(`Host %s
  # ssm-region: %s
  # ssm-profile: %s
  HostName %s
  User %s
  IdentityFile ~/.ssh/id_ed25519
  ProxyCommand aws ec2-instance-connect open-tunnel --instance-id %%h --region %s --profile %s
  ControlMaster auto
  ControlPath ~/.ssh/ctrl-%%C
  ControlPersist 4h
%s`, name, region, profile, instanceID, osUser, region, profile, extra)

	return os.WriteFile(configPath, []byte(rest+block), 0600)
}

// HostMeta holds ssm metadata stored as comments in a Host block.
type HostMeta struct {
	Region  string
	Profile string
	User    string
}

// LookupHost reads ssm-region and ssm-profile from a named Host block in ~/.ssh/config.
// Returns zero value if the host or fields are not found.
func LookupHost(name string) HostMeta {
	home, err := os.UserHomeDir()
	if err != nil {
		return HostMeta{}
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return HostMeta{}
	}
	var meta HostMeta
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "Host "); ok {
			inBlock = rest == name
			continue
		}
		if !inBlock {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "# ssm-region: "); ok {
			meta.Region = v
		} else if v, ok := strings.CutPrefix(trimmed, "# ssm-profile: "); ok {
			meta.Profile = v
		} else if v, ok := strings.CutPrefix(trimmed, "User "); ok {
			meta.User = v
		}
	}
	return meta
}

// removeHostBlock removes an existing Host block for the given name from config content.
// Returns the content outside the block and any "extra" lines from inside the block that
// are not managed by ssm (e.g. LocalForward), so callers can preserve them.
func removeHostBlock(content, name string) (rest, extra string) {
	managedPrefixes := []string{
		"# ssm-region:", "# ssm-profile:",
		"HostName ", "User ", "IdentityFile ", "ProxyCommand ",
		"ControlMaster ", "ControlPath ", "ControlPersist ",
	}
	isManaged := func(line string) bool {
		t := strings.TrimSpace(line)
		for _, p := range managedPrefixes {
			if strings.HasPrefix(t, p) {
				return true
			}
		}
		return false
	}

	var outLines, extraLines []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "Host "); ok {
			inBlock = rest == name
		}
		if !inBlock {
			outLines = append(outLines, line)
		} else if inBlock && !strings.HasPrefix(trimmed, "Host ") && !isManaged(line) && trimmed != "" {
			extraLines = append(extraLines, "  "+trimmed)
		}
	}

	result := strings.Join(outLines, "\n")
	if !strings.HasSuffix(result, "\n") && len(result) > 0 {
		result += "\n"
	}
	if len(extraLines) > 0 {
		extra = strings.Join(extraLines, "\n") + "\n"
	}
	return result, extra
}
