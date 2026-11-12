package sessionhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// prepareCheckout makes dir a checkout of url on branch: a clone if it is not one yet, otherwise a
// fast-forward when the tree is clean and on that branch. A dirty or diverged checkout is left as
// the user left it (the session may be mid-work); only a failed clone is an error. The checkout is
// the session's own, never the dev checkout.
func prepareCheckout(dir, url, branch, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		args := []string{"clone"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		if out, err := git(ctx, "", token, append(args, "--", url, dir)...); err != nil {
			return fmt.Errorf("clone %s: %v: %s", url, err, out)
		}
		return nil
	}
	if branch == "" {
		return nil
	}
	if out, err := git(ctx, dir, token, "fetch", "origin", branch); err != nil {
		_ = out
		return nil // offline or token trouble: start with what is there
	}
	if cur, _ := git(ctx, dir, "", "rev-parse", "--abbrev-ref", "HEAD"); cur != branch {
		return nil
	}
	if st, err := git(ctx, dir, "", "status", "--porcelain"); err != nil || st != "" {
		return nil
	}
	git(ctx, dir, "", "merge", "--ff-only", "FETCH_HEAD")
	return nil
}

// git runs git with the token, if any, as an http.extraheader in the environment: never on the
// command line and never on disk.
func git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return s, err
}
