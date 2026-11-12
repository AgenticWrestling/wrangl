package devbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostBlock describes one `Host` entry written to ~/.ssh/config.
type HostBlock struct {
	Name       string // the alias: `ssh NAME`
	InstanceID string
	Region     string
	Profile    string
	User       string
	Wrangl     string // path of the wrangl binary used as ProxyCommand
}

// HostMeta is the metadata kept as comments in a managed Host block.
type HostMeta struct{ Region, Profile, User, InstanceID string }

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t\"'\\$") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// Render returns the Host block. The ProxyCommand is `wrangl dev proxy`, which pushes a fresh
// EC2 Instance Connect key (valid 60 s) every time ssh connects and then opens the tunnel, so
// plain `ssh NAME`, scp and editors keep working after the first minute.
func (b HostBlock) Render() string {
	w := b.Wrangl
	if w == "" {
		w = "wrangl"
	}
	var p []string
	p = append(p, quote(w), "dev", "proxy", "%h", "--user", "%r")
	if b.Region != "" {
		p = append(p, "--region", b.Region)
	}
	if b.Profile != "" {
		p = append(p, "--profile", b.Profile)
	}
	return fmt.Sprintf(`Host %s
  # wrangl-region: %s
  # wrangl-profile: %s
  HostName %s
  User %s
  IdentityFile ~/.ssh/%s
  IdentitiesOnly yes
  ProxyCommand %s
  ControlMaster auto
  ControlPath ~/.ssh/ctrl-%%C
  ControlPersist 4h
`, b.Name, b.Region, b.Profile, b.InstanceID, b.User, KeyName, strings.Join(p, " "))
}

// UpsertHostBlock writes (or replaces) the Host block, keeping lines inside an existing block
// that wrangl does not manage (for example LocalForward).
func UpsertHostBlock(b HostBlock) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var rest, extra string
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		rest, extra = removeHostBlock(string(existing), b.Name)
	}
	return os.WriteFile(path, []byte(rest+b.Render()+extra), 0o600)
}

// LookupHost reads the metadata of a managed Host block.
// A zero value means the host is not found.
func LookupHost(name string) HostMeta {
	path, err := configPath()
	if err != nil {
		return HostMeta{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return HostMeta{}
	}
	var m HostMeta
	in := false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(t, "Host "); ok {
			in = rest == name
			continue
		}
		if !in {
			continue
		}
		for _, p := range []struct {
			prefix string
			dst    *string
		}{{"# wrangl-region: ", &m.Region}, {"# wrangl-profile: ", &m.Profile}, {"User ", &m.User}, {"HostName ", &m.InstanceID}} {
			if v, ok := strings.CutPrefix(t, p.prefix); ok {
				*p.dst = v
			}
		}
	}
	return m
}

// removeHostBlock cuts the named block out of content and returns the rest plus the block's
// unmanaged lines.
func removeHostBlock(content, name string) (rest, extra string) {
	managed := []string{"# wrangl-region:", "# wrangl-profile:", "HostName ", "User ", "IdentityFile ", "IdentitiesOnly ", "ProxyCommand ", "ControlMaster ", "ControlPath ", "ControlPersist "}
	isManaged := func(t string) bool {
		for _, p := range managed {
			if strings.HasPrefix(t, p) {
				return true
			}
		}
		return false
	}
	var out, ex []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if h, ok := strings.CutPrefix(t, "Host "); ok {
			in = h == name
		}
		switch {
		case !in:
			out = append(out, line)
		case !strings.HasPrefix(t, "Host ") && !isManaged(t) && t != "":
			ex = append(ex, "  "+t)
		}
	}
	rest = strings.Join(out, "\n")
	if rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	if len(ex) > 0 {
		extra = strings.Join(ex, "\n") + "\n"
	}
	return rest, extra
}
