package sessionhost

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Config configures a Host. The zero value runs `claude --remote-control <name> --name <name>`.
type Config struct {
	// Argv builds the command for a session; resume is true when the session is being started
	// again after it ended or was restarted (Claude continues with -c).
	Argv      func(sp Spec, resume bool) []string
	Env       []string // extra environment (the process environment is inherited)
	Cols      int      // initial size (default 120x40)
	Rows      int
	RingBytes int           // raw scrollback kept per session (default 256 KiB)
	StopGrace time.Duration // SIGHUP to SIGKILL (default 5s)
	MaxSess   int           // default 32
	NewScreen func(cols, rows int, reply func([]byte)) Screen
}

func (c *Config) defaults() {
	if c.Argv == nil {
		c.Argv = func(sp Spec, resume bool) []string {
			bin, err := exec.LookPath("claude")
			if err != nil {
				bin = "claude"
			}
			a := []string{bin, "--remote-control", sp.Name, "--name", sp.Name}
			if resume {
				a = append(a, "-c")
			}
			return a
		}
	}
	if c.Cols == 0 {
		c.Cols, c.Rows = 120, 40
	}
	if c.RingBytes == 0 {
		c.RingBytes = 256 << 10
	}
	if c.StopGrace == 0 {
		c.StopGrace = 5 * time.Second
	}
	if c.MaxSess == 0 {
		c.MaxSess = 32
	}
	if c.NewScreen == nil {
		c.NewScreen = NewVTScreen
	}
}

// Host is the registry of sessions.
type Host struct {
	cfg  Config
	mu   sync.Mutex
	sess map[string]*session
}

// NewHost returns an empty host.
func NewHost(cfg Config) *Host {
	cfg.defaults()
	return &Host{cfg: cfg, sess: map[string]*session{}}
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (h *Host) env() []string {
	env := append(os.Environ(), "TERM=xterm-256color")
	return append(env, h.cfg.Env...)
}

// Start runs a session for the slug in dir. It is idempotent: a running session is returned as is
// (existing=true). One that has ended is started again with resume.
func (h *Host) Start(slug, dir, name string) (info Info, existing bool, err error) {
	return h.StartWith(Request{Slug: slug, Dir: dir, Name: name})
}

// StartWith is Start with the optional worktree preparation and resume of the request.
func (h *Host) StartWith(req Request) (info Info, existing bool, err error) {
	slug, dir, name := req.Slug, req.Dir, req.Name
	if !slugRE.MatchString(slug) {
		return Info{}, false, fmt.Errorf("invalid slug %q", slug)
	}
	if name == "" {
		name = slug
	}
	if !filepath.IsAbs(dir) {
		return Info{}, false, fmt.Errorf("dir %q must be absolute", dir)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	resume := req.Resume
	if old := h.sess[slug]; old != nil {
		if i := old.snapshotInfo(); i.State == "running" {
			return i, true, nil
		}
		resume = true
	} else if len(h.sess) >= h.cfg.MaxSess {
		return Info{}, false, fmt.Errorf("at the limit of %d sessions", h.cfg.MaxSess)
	}
	if req.CloneURL != "" {
		if err := prepareCheckout(dir, req.CloneURL, req.Branch, req.Token); err != nil {
			return Info{}, false, err
		}
	}
	if st, e := os.Stat(dir); e != nil || !st.IsDir() {
		return Info{}, false, fmt.Errorf("dir %q is not a directory", dir)
	}
	s, err := h.spawn(Spec{slug, dir, name}, resume)
	if err != nil {
		return Info{}, false, err
	}
	h.sess[slug] = s
	return s.snapshotInfo(), false, nil
}

func (h *Host) spawn(sp Spec, resume bool) (*session, error) {
	return startSession(sp, h.cfg.Argv(sp, resume), h.env(), resume, h.cfg.Cols, h.cfg.Rows, h.cfg.RingBytes, h.cfg.NewScreen)
}

var errNoSession = errors.New("no such session")

// Stop ends the session and forgets it.
func (h *Host) Stop(slug string) error {
	h.mu.Lock()
	s := h.sess[slug]
	delete(h.sess, slug)
	h.mu.Unlock()
	if s == nil {
		return errNoSession
	}
	s.stop(h.cfg.StopGrace)
	return nil
}

// Restart ends the session and starts it again with resume (-c). Attached clients are sent the end
// frame and must reattach.
func (h *Host) Restart(slug string) (Info, error) {
	h.mu.Lock()
	old := h.sess[slug]
	h.mu.Unlock()
	if old == nil {
		return Info{}, errNoSession
	}
	old.stop(h.cfg.StopGrace)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess[slug] != old { // stopped or restarted by someone else meanwhile
		return Info{}, errNoSession
	}
	i := old.info
	s, err := h.spawn(Spec{i.Slug, i.Dir, i.Name}, true)
	if err != nil {
		return Info{}, err
	}
	h.sess[slug] = s
	return s.snapshotInfo(), nil
}

// List returns every session, by slug.
func (h *Host) List() []Info {
	h.mu.Lock()
	ss := make([]*session, 0, len(h.sess))
	for _, s := range h.sess {
		ss = append(ss, s)
	}
	h.mu.Unlock()
	out := make([]Info, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.snapshotInfo())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func (h *Host) get(slug string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess[slug]
}

// StopAll ends every session (used by tests and a clean service stop; a service restart that wants
// sessions to survive must not call it, and they die with the process either way).
func (h *Host) StopAll() {
	for _, i := range h.List() {
		h.Stop(i.Slug)
	}
}
