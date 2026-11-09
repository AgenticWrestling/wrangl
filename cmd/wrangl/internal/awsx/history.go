package awsx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

type history struct {
	Profiles []string `json:"profiles"`
}

func historyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "wrangl", "dev-history.json"), nil
}

func readHistory(path string) (history, bool) {
	var h history
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &h) != nil {
		return history{}, false
	}
	return h, true
}

// SortProfilesByMRU reorders profiles so the most-recently-used appear first.
// Profiles not in history retain their original relative order at the end.
func SortProfilesByMRU(profiles []string) []string {
	path, err := historyPath()
	if err != nil {
		return profiles
	}
	h, ok := readHistory(path)
	if !ok {
		return profiles
	}

	seen := make(map[string]bool)
	var sorted []string
	for _, p := range h.Profiles {
		if slices.Contains(profiles, p) {
			sorted = append(sorted, p)
			seen[p] = true
		}
	}
	for _, p := range profiles {
		if !seen[p] {
			sorted = append(sorted, p)
		}
	}
	return sorted
}

// RecordProfileUsage saves profile as the most-recently-used, capped at 20 entries.
func RecordProfileUsage(profile string) error {
	path, err := historyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	h, _ := readHistory(path)

	// Move profile to front.
	h.Profiles = slices.DeleteFunc(h.Profiles, func(p string) bool { return p == profile })
	h.Profiles = append([]string{profile}, h.Profiles...)
	if len(h.Profiles) > 20 {
		h.Profiles = h.Profiles[:20]
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
