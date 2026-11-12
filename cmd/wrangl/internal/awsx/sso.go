package awsx

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sts"
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
// For non-SSO profiles without a role_arn, a short STS GetCallerIdentity call
// is attempted to populate AccountID.
func LoadProfileInfo(ctx context.Context, profile string) ProfileInfo {
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

	// For role_arn profiles the account ID is embedded in the ARN.
	if info.AccountID == "" {
		if k, err := section.GetKey("role_arn"); err == nil {
			info.AccountID = accountIDFromARN(k.String())
		}
	}

	// For plain IAM profiles (no SSO, no role_arn), try STS with a short timeout.
	if info.AccountID == "" && !info.IsSSO {
		info.AccountID = stsAccountID(ctx, profile, info.Region)
	}

	return info
}

// accountIDFromARN extracts the account ID from an ARN like
// arn:aws:iam::123456789012:role/MyRole.
func accountIDFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) >= 5 {
		return parts[4]
	}
	return ""
}

// stsAccountID calls GetCallerIdentity with a 3-second timeout.
// Returns empty string on any error.
func stsAccountID(ctx context.Context, profile, region string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	awsCfg, err := LoadConfig(ctx, profile, region)
	if err != nil {
		return ""
	}
	out, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, nil)
	if err != nil || out.Account == nil {
		return ""
	}
	return *out.Account
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

// profileKey reads one key of a profile section in ~/.aws/config ("" if absent).
func profileKey(profile, key string) string {
	path := os.Getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, ".aws", "config")
	}
	cfg, err := ini.Load(path)
	if err != nil {
		return ""
	}
	name := "profile " + profile
	if profile == "default" || profile == "" {
		name = "default"
	}
	sec, err := cfg.GetSection(name)
	if err != nil {
		return ""
	}
	k, err := sec.GetKey(key)
	if err != nil {
		return ""
	}
	return k.String()
}

// IsSSOExpired reports whether err is the SDK's "SSO token expired / not refreshable" error.
func IsSSOExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "InvalidGrantException") ||
		strings.Contains(s, "refresh cached SSO token") ||
		strings.Contains(s, "login session has expired") ||
		strings.Contains(s, "please reauthenticate")
}

// FriendlyError turns noisy SDK errors into an actionable message.
func FriendlyError(err error) string {
	if IsSSOExpired(err) {
		return "AWS SSO session expired. Run: aws sso login --profile <profile>"
	}
	return err.Error()
}

// RefreshSSO runs `aws sso login --profile P` interactively.
func RefreshSSO(profile string) error {
	awsPath, err := exec.LookPath("aws")
	if err != nil {
		return fmt.Errorf("aws CLI not found in PATH: %w", err)
	}
	cmd := exec.Command(awsPath, "sso", "login", "--profile", profile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// EnsureSSO refreshes the profile's SSO token first when the cache says it expired.
func EnsureSSO(profile string) error {
	if expired, _ := SSOTokenExpired(profile); expired {
		fmt.Fprintf(os.Stderr, "SSO session expired for profile %q. Refreshing...\n", profile)
		if err := RefreshSSO(profile); err != nil {
			return fmt.Errorf("sso login: %w", err)
		}
	}
	return nil
}
