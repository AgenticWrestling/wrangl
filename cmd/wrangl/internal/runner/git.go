package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// gitAsDaemon runs git as the daemon (root in production). The installation token, if
// any, is passed as an http.extraheader through GIT_CONFIG_* environment variables: never
// on the command line (visible in ps) and never on disk. The agent user cannot read a
// root process's environment.
func gitAsDaemon(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cfg := [][2]string{{"safe.directory", "*"}} // the worktree is owned by the agent user
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cfg = append(cfg, [2]string{"http.extraheader", "AUTHORIZATION: basic " + basic})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), redact(err, token)
}

// gitAsAgent runs git as the agent identity, with no credentials.
func gitAsAgent(ctx context.Context, id agentIdentity, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + id.home, "USER=" + id.username, "PATH=/usr/local/bin:/usr/bin:/bin", "GIT_TERMINAL_PROMPT=0"}
	if id.drop {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(id.uid), Gid: uint32(id.gid)}}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), secret, "***"))
}

// chownTree hands a directory tree to the agent user (no-op unless dropping privileges).
func chownTree(root string, id agentIdentity) error {
	if !id.drop {
		return nil
	}
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, id.uid, id.gid)
	})
}
