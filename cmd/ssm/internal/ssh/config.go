// cmd/ssm/internal/ssh/config.go
package ssh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func UpsertHostBlock(name, instanceID, region, profile string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")

	block := fmt.Sprintf(`Host %s
  HostName %s
  User ec2-user
  IdentityFile ~/.ssh/id_ed25519
  ProxyCommand aws ec2-instance-connect open-tunnel --instance-id %%h --region %s --profile %s
`, name, instanceID, region, profile)

	existing, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if err == nil {
		out := removeHostBlock(string(existing), name)
		return os.WriteFile(configPath, []byte(out+block), 0600)
	}
	return os.WriteFile(configPath, []byte(block), 0600)
}

// removeHostBlock removes an existing Host block for the given name from config content.
func removeHostBlock(content, name string) string {
	var out []string
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Host ") {
			inBlock = strings.TrimPrefix(trimmed, "Host ") == name
		}
		if !inBlock {
			out = append(out, line)
		}
	}
	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") && len(result) > 0 {
		result += "\n"
	}
	return result
}
