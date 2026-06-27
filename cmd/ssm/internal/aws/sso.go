package aws

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

// ProfileInfo holds display metadata for a profile, read from local config/cache.
type ProfileInfo struct {
	Name      string
	AccountID string // sso_account_id
	Role      string // sso_role_name
	Region    string // region
	OrgHint   string // subdomain of sso_start_url
	TokenOK   bool   // false if SSO token missing/expired (or not SSO)
	IsSSO     bool
}

// LoadProfileInfo reads metadata for a profile from ~/.aws/config and SSO cache.
// Never returns an error — missing fields are left empty.
func LoadProfileInfo(profile string) ProfileInfo {
	info := ProfileInfo{Name: profile}

	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".aws", "config")
		}
	}
	cfg, err := ini.Load(path)
	if err != nil {
		return info
	}

	sectionName := "profile " + profile
	if profile == "default" {
		sectionName = "default"
	}
	section, err := cfg.GetSection(sectionName)
	if err != nil {
		return info
	}

	if k, err := section.GetKey("sso_account_id"); err == nil {
		info.AccountID = k.String()
	}
	if k, err := section.GetKey("sso_role_name"); err == nil {
		info.Role = k.String()
	}
	if k, err := section.GetKey("region"); err == nil {
		info.Region = k.String()
	}
	if k, err := section.GetKey("sso_start_url"); err == nil {
		startURL := k.String()
		info.IsSSO = true
		info.OrgHint = ssoSubdomain(startURL)
		info.TokenOK = !tokenExpired(startURL)
	}

	return info
}

// SSOTokenExpired returns true if the SSO token for the given profile is
// missing or expired.
func SSOTokenExpired(profile string) (bool, error) {
	startURL, err := profileSSOStartURL(profile)
	if err != nil || startURL == "" {
		return false, nil
	}
	return tokenExpired(startURL), nil
}

func tokenExpired(startURL string) bool {
	cacheFile, err := ssoCacheFile(startURL)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return true
	}
	var token struct {
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &token); err != nil || token.ExpiresAt == "" {
		return true
	}
	exp, err := time.Parse(time.RFC3339, token.ExpiresAt)
	if err != nil {
		return true
	}
	return time.Now().After(exp)
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
		return "", nil
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

// ssoSubdomain extracts just the subdomain from an SSO start URL.
// e.g. "https://myorg.awsapps.com/start" → "myorg"
func ssoSubdomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if idx := strings.Index(host, "."); idx > 0 {
		return host[:idx]
	}
	return host
}
