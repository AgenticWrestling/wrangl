package aws

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/ini.v1"
)

// SSOTokenExpired returns true if the SSO token for the given profile is
// missing or expired. It reads the profile's sso_start_url from ~/.aws/config,
// then checks the matching cache file in ~/.aws/sso/cache/.
func SSOTokenExpired(profile string) (bool, error) {
	startURL, err := profileSSOStartURL(profile)
	if err != nil || startURL == "" {
		// Not an SSO profile — nothing to check.
		return false, nil
	}

	cacheFile, err := ssoCacheFile(startURL)
	if err != nil {
		return true, nil // can't find cache → treat as expired
	}

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return true, nil // missing cache → expired
	}

	var token struct {
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &token); err != nil || token.ExpiresAt == "" {
		return true, nil
	}

	exp, err := time.Parse(time.RFC3339, token.ExpiresAt)
	if err != nil {
		return true, nil
	}
	return time.Now().After(exp), nil
}

func profileSSOStartURL(profile string) (string, error) {
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".aws", "config")
	}

	cfg, err := ini.Load(path)
	if err != nil {
		return "", err
	}

	sectionName := "profile " + profile
	if profile == "default" {
		sectionName = "default"
	}
	section, err := cfg.GetSection(sectionName)
	if err != nil {
		return "", fmt.Errorf("profile %q not found in config", profile)
	}
	key, err := section.GetKey("sso_start_url")
	if err != nil {
		return "", nil // not an SSO profile
	}
	return key.String(), nil
}

// ssoCacheFile returns the path to the SSO cache file for a given start URL.
// AWS CLI derives the filename as sha1(startURL) + ".json".
func ssoCacheFile(startURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	hash := fmt.Sprintf("%x", sha1.Sum([]byte(startURL)))
	return filepath.Join(home, ".aws", "sso", "cache", hash+".json"), nil
}
