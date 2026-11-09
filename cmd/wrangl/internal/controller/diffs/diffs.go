// Package diffs serves the data behind the review card's diff: the changed files with per-file unified
// diffs and the checkpoint commits of a run or PR. It reads a bare mirror of the repo kept on the controller
// (git diff), refreshing it when it lacks the commits, and falls back to GitHub's pulls API only if the
// mirror cannot supply them (e.g. the branch was force-pushed away or deleted).
package diffs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Caps. A file with more changed lines than MaxFileLines has its patch omitted; once the patches in one
// response reach MaxPayload the rest are omitted too. Both are flagged Truncated with a GitHub link.
const (
	MaxFileLines = 500
	MaxPayload   = 1 << 20
	maxGitOutput = 16 << 20
)

var shaRE = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// Service computes diff views.
type Service struct {
	DB *db.DB
	// Dir holds the bare mirrors, <Dir>/<owner>/<name>.git, created on first use. Empty disables the
	// mirror (GitHub only).
	Dir string
	// Token mints a git token for a repo (nil: public fetches only).
	Token func(ctx context.Context, repo dbgen.Repo) (string, error)
	// GH returns the GitHub client for a repo (nil: no fallback).
	GH func(gh.Repo) gh.PRAPI

	locks sync.Map
}

// ForRun is the diff of a run's base..head.
func (s *Service) ForRun(ctx context.Context, runID int64, commit string) (proto.DiffView, error) {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.DiffView{}, fmt.Errorf("%w: run %d", errs.NotFound, runID)
	} else if err != nil {
		return proto.DiffView{}, err
	}
	if !run.RepoID.Valid {
		return proto.DiffView{}, fmt.Errorf("%w: run %d has no repository", errs.Conflict, runID)
	}
	if run.BaseSha == "" || run.HeadSha == "" {
		return proto.DiffView{}, fmt.Errorf("%w: run %d has no commits yet", errs.Conflict, runID)
	}
	repo, err := s.DB.RQ.GetRepo(ctx, run.RepoID.Int64)
	if err != nil {
		return proto.DiffView{}, err
	}
	var pr int64
	if run.PrID.Valid {
		if p, err := s.DB.RQ.GetPR(ctx, run.PrID.Int64); err == nil {
			pr = p.GhNumber
		}
	}
	return s.view(ctx, repo, run.BaseSha, run.HeadSha, commit, pr)
}

// ForPR is the diff of a PR: from the base its run started at (else the merge base with the default
// branch) to the PR's head.
func (s *Service) ForPR(ctx context.Context, prID int64, commit string) (proto.DiffView, error) {
	pr, err := s.DB.RQ.GetPR(ctx, prID)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.DiffView{}, fmt.Errorf("%w: PR %d", errs.NotFound, prID)
	} else if err != nil {
		return proto.DiffView{}, err
	}
	repo, err := s.DB.RQ.GetRepo(ctx, pr.RepoID)
	if err != nil {
		return proto.DiffView{}, err
	}
	base := ""
	if run, err := s.DB.RQ.GetRun(ctx, pr.RunID); err == nil {
		base = run.BaseSha
	}
	return s.view(ctx, repo, base, pr.HeadSha, commit, pr.GhNumber)
}

func (s *Service) view(ctx context.Context, repo dbgen.Repo, base, head, commit string, pr int64) (proto.DiffView, error) {
	if commit != "" && !shaRE.MatchString(commit) {
		return proto.DiffView{}, fmt.Errorf("%w: commit %q", errs.Invalid, commit)
	}
	ghr := projects.GHRepo(repo)
	base = strings.ToLower(base)
	head = strings.ToLower(head)
	v, err := s.fromMirror(ctx, repo, base, head, strings.ToLower(commit))
	if err == nil {
		v.Link = changeLink(ghr, pr, v.Base, v.Head, v.Commit)
		applyCaps(&v, ghr, pr)
		return v, nil
	}
	if errors.Is(err, errs.Invalid) || errors.Is(err, errs.NotFound) {
		return proto.DiffView{}, err
	}
	// Fallback: GitHub, whole PR only.
	if pr == 0 || commit != "" || s.GH == nil {
		return proto.DiffView{}, fmt.Errorf("%w: the mirror cannot supply these commits: %v", errs.Conflict, err)
	}
	api := s.GH(ghr)
	if api == nil {
		return proto.DiffView{}, fmt.Errorf("%w: the mirror cannot supply these commits: %v", errs.Conflict, err)
	}
	text, gerr := api.PRDiff(ctx, ghr, pr)
	if gerr != nil {
		return proto.DiffView{}, fmt.Errorf("mirror: %v; github: %w", err, gerr)
	}
	v = proto.DiffView{Base: base, Head: head, Source: "github", Files: parseDiff(text), Commits: []proto.DiffCommit{}}
	v.Link = changeLink(ghr, pr, base, head, "")
	applyCaps(&v, ghr, pr)
	return v, nil
}

func changeLink(r gh.Repo, pr int64, base, head, commit string) string {
	root := fmt.Sprintf("https://github.com/%s/%s", r.Owner, r.Name)
	switch {
	case commit != "":
		return root + "/commit/" + commit
	case pr > 0:
		return fmt.Sprintf("%s/pull/%d/files", root, pr)
	case base != "" && head != "":
		return fmt.Sprintf("%s/compare/%s...%s", root, base, head)
	}
	return root
}

// applyCaps omits oversized patches and flags them; binary files carry no patch at all.
func applyCaps(v *proto.DiffView, r gh.Repo, pr int64) {
	total := 0
	for i := range v.Files {
		f := &v.Files[i]
		if f.Binary {
			f.Patch = ""
			continue
		}
		if f.Additions+f.Deletions > MaxFileLines || total+len(f.Patch) > MaxPayload {
			f.Patch, f.Truncated = "", true
			v.Truncated = true
			f.Link = v.Link
			if pr > 0 {
				f.Link = fmt.Sprintf("https://github.com/%s/%s/pull/%d/files", r.Owner, r.Name, pr)
			}
			continue
		}
		total += len(f.Patch)
	}
	if v.Files == nil {
		v.Files = []proto.DiffFile{}
	}
}

// --- mirror --------------------------------------------------------------------------

func (s *Service) mirrorPath(r dbgen.Repo) string {
	return filepath.Join(s.Dir, filepath.Base(r.Owner), filepath.Base(r.Name)+".git")
}

func (s *Service) fromMirror(ctx context.Context, repo dbgen.Repo, base, head, commit string) (proto.DiffView, error) {
	if s.Dir == "" {
		return proto.DiffView{}, errors.New("no mirror configured")
	}
	dir := s.mirrorPath(repo)
	if err := s.ensure(ctx, repo, dir, head, base); err != nil {
		return proto.DiffView{}, err
	}
	if base == "" { // PR without a recorded run base
		out, err := git(ctx, dir, "", "merge-base", "refs/remotes/origin/"+repo.DefaultBranch, head)
		if err != nil {
			return proto.DiffView{}, fmt.Errorf("merge-base: %w", err)
		}
		base = strings.TrimSpace(out)
	}
	v := proto.DiffView{Base: base, Head: head, Source: "mirror", Commits: []proto.DiffCommit{}}
	log, err := git(ctx, dir, "", "log", "--reverse", "--format=%H%x1f%an%x1f%aI%x1f%s", base+".."+head)
	if err != nil {
		return v, err
	}
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if p := strings.Split(l, "\x1f"); len(p) == 4 {
			v.Commits = append(v.Commits, proto.DiffCommit{SHA: p[0], Author: p[1], Date: p[2], Subject: p[3]})
		}
	}
	from, to := base, head
	if commit != "" {
		full := ""
		for _, c := range v.Commits {
			if strings.HasPrefix(c.SHA, commit) {
				full = c.SHA
			}
		}
		if full == "" {
			return v, fmt.Errorf("%w: commit %s is not part of this change", errs.NotFound, commit)
		}
		v.Commit, from, to = full, full+"~1", full
	}
	v.Base, v.Head = from, to
	if commit == "" {
		v.Base, v.Head = base, head
	}
	out, err := git(ctx, dir, "", "diff", "--no-color", "--no-ext-diff", "--find-renames", from, to)
	if err != nil {
		return v, err
	}
	v.Files = parseDiff(out)
	return v, nil
}

// ensure makes the mirror contain the commits, cloning or fetching as needed.
func (s *Service) ensure(ctx context.Context, repo dbgen.Repo, dir string, shas ...string) error {
	has := func() bool {
		for _, sha := range shas {
			if sha == "" {
				continue
			}
			if _, err := git(ctx, dir, "", "cat-file", "-e", sha+"^{commit}"); err != nil {
				return false
			}
		}
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil && has() {
		return nil
	}
	mu, _ := s.locks.LoadOrStore(dir, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	tok := ""
	if s.Token != nil {
		tok, _ = s.Token(ctx, repo)
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return err
		}
		if out, err := git(ctx, filepath.Dir(dir), tok, "init", "--bare", dir); err != nil {
			return fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	git(ctx, dir, "", "remote", "remove", "origin")
	if out, err := git(ctx, dir, "", "remote", "add", "origin", repo.CloneUrl); err != nil {
		return fmt.Errorf("git remote add: %v: %s", err, out)
	}
	if out, err := git(ctx, dir, tok, "fetch", "--prune", "origin",
		"+refs/heads/*:refs/remotes/origin/*", "+refs/pull/*/head:refs/pull/*/head"); err != nil {
		return fmt.Errorf("git fetch: %v: %s", err, out)
	}
	if !has() {
		return errors.New("mirror does not have the commits")
	}
	return nil
}

func git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cfg := [][2]string{{"safe.directory", "*"}}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cfg = append(cfg, [2]string{"http.extraheader", "AUTHORIZATION: basic " + basic})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	cmd.Env = env
	var out, serr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitWriter{w: &out, n: maxGitOutput}, &serr
	err := cmd.Run()
	res := out.String()
	if err != nil {
		res += serr.String()
	}
	if token != "" {
		res = strings.ReplaceAll(res, token, "***")
		if err != nil {
			err = errors.New(strings.ReplaceAll(err.Error(), token, "***"))
		}
	}
	return res, err
}

// limitWriter drops output beyond n (the diff is then cut; per-file caps flag what matters).
type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
