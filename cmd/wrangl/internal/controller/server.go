package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/nreid/devhost/cmd/wrangl/internal/api"
	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/fleet"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/gh"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/projects"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/provision"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/prs"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/runs"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/scheduler"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/events"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

// JetStream streams (spec §7).
const (
	StreamEvents = "WR_EVT"
	StreamPTY    = "WR_PTY"
)

// Streams returns the stream configs the controller ensures on start.
func Streams() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{Name: StreamEvents, Subjects: []string{"wr.evt.>"}, Storage: jetstream.FileStorage, MaxAge: 7 * 24 * time.Hour},
		{Name: StreamPTY, Subjects: []string{"wr.pty.>"}, Storage: jetstream.FileStorage, MaxAge: 3 * 24 * time.Hour, MaxBytes: 2 << 30},
	}
}

// Server is the controller process.
type Server struct {
	cfg   *config.Controller
	log   *slog.Logger
	ready chan struct{}

	// Test hooks; production leaves them zero.
	Provisioner         *provision.Provisioner // overrides the AWS-backed one
	GH                  gh.API                 // overrides the GitHub App client built from config
	Reviewer            gh.API                 // overrides the reviewer App client built from config
	ReconcileInterval   time.Duration          // PR reconcile poll (default 15m)
	Now                 func() time.Time
	LoopInterval        time.Duration // offline sweep + reaper cadence (default 30s)
	SchedulerInterval   time.Duration // scale-up cadence (default 10s)
	AllowLoopbackEnroll bool

	mu         sync.Mutex
	httpAddr   string
	enrollAddr string
	natsURL    string

	db       *db.DB
	nats     *natsd.Embedded
	js       jetstream.JetStream
	Fleet    *fleet.Service
	Stack    *stack.Service
	Projects *projects.Service
	Runs     *runs.Service
	PRs      *prs.Service
	Bus      *events.Bus
}

// New validates cfg and returns an unstarted server.
func New(cfg *config.Controller, log *slog.Logger) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("data_dir must be set")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log, ready: make(chan struct{})}, nil
}

// Ready is closed once every listener is accepting.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// HTTPAddr is the bound API address (valid after Ready).
func (s *Server) HTTPAddr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.httpAddr }

// EnrollAddr is the bound enrol-listener address (valid after Ready).
func (s *Server) EnrollAddr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.enrollAddr }

// NATSURL is the TLS URL runners connect to (valid after Ready).
func (s *Server) NATSURL() string { s.mu.Lock(); defer s.mu.Unlock(); return s.natsURL }

// NATSCA is the PEM CA runners pin (valid after Ready).
func (s *Server) NATSCA() []byte { return s.nats.CA.CertPEM }

// awsProvisioner builds the EC2 client from the default credential chain: the instance
// role on the controller host, no keys in config. Nil when aws.region is unset.
func (s *Server) awsProvisioner(ctx context.Context, controllerID string) (*provision.Provisioner, error) {
	if s.Provisioner != nil {
		if s.Provisioner.ControllerID == "" {
			s.Provisioner.ControllerID = controllerID
		}
		return s.Provisioner, nil
	}
	if s.cfg.AWS.Region == "" {
		s.log.Warn("aws.region not set: launching and reaping runners is disabled")
		return nil, nil
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(s.cfg.AWS.Region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &provision.Provisioner{EC2: ec2.NewFromConfig(ac), SSM: ssm.NewFromConfig(ac), ControllerID: controllerID}, nil
}

// Run starts everything, blocks until ctx is cancelled, then shuts down HTTP, NATS and
// the database in that order.
func (s *Server) Run(ctx context.Context) (err error) {
	if s.db, err = db.Open(ctx, DBPath(s.cfg.DataDir)); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.db.Close()) }()
	controllerID, err := s.db.RQ.GetMeta(ctx, MetaControllerID)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("no controller id: run `wrangl controller init` first")
	} else if err != nil {
		return err
	}

	if s.nats, err = natsd.Start(s.cfg.DataDir, s.cfg.Listen.NATS); err != nil {
		return err
	}
	defer s.nats.Close()
	if s.js, err = jetstream.New(s.nats.Conn); err != nil {
		return err
	}
	for _, sc := range Streams() {
		if _, err := s.js.CreateOrUpdateStream(ctx, sc); err != nil {
			return fmt.Errorf("ensure stream %s: %w", sc.Name, err)
		}
	}

	// Listeners first, so the advertised URLs carry real ports.
	apiLn, err := net.Listen("tcp", s.cfg.Listen.HTTP)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	enrollLn, err := net.Listen("tcp", s.cfg.Listen.Enroll)
	if err != nil {
		apiLn.Close()
		return fmt.Errorf("enroll listen: %w", err)
	}
	listeners := []net.Listener{apiLn}
	if s.cfg.Listen.Unix != "" {
		unix, err := listenUnix(s.cfg.Listen.Unix)
		if err != nil {
			apiLn.Close()
			enrollLn.Close()
			return err
		}
		listeners = append(listeners, unix)
	}

	prov, err := s.awsProvisioner(ctx, controllerID)
	if err != nil {
		return err
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	subnet := ""
	if len(s.cfg.AWS.SubnetIDs) > 0 {
		subnet = s.cfg.AWS.SubnetIDs[0]
	}
	s.Fleet = &fleet.Service{
		DB: s.db, Keys: s.nats.Keys, NC: s.nats.Conn, NATSCA: s.nats.CA.CertPEM, NATSURLs: []string{s.nats.URL()},
		Prov: prov, ControllerID: controllerID, EnrollURL: "http://" + enrollLn.Addr().String(),
		SubnetID: subnet, SecurityGroupID: s.cfg.AWS.SecurityGroupID, InstanceProfile: s.cfg.AWS.RunnerInstanceProfile,
		BinDir: filepath.Join(s.cfg.DataDir, "bin"), Now: now, Log: s.log, AllowLoopbackEnroll: s.AllowLoopbackEnroll,
	}

	ghAPI, err := s.github()
	if err != nil {
		return err
	}
	alpha, err := s.db.RQ.GetMeta(ctx, MetaSqidsAlphabet)
	if err != nil {
		return fmt.Errorf("sqids alphabet: %w", err)
	}
	sq, err := stack.NewSqids(alpha)
	if err != nil {
		return err
	}
	s.Bus = &events.Bus{}
	s.Fleet.Bus = s.Bus
	s.Stack = &stack.Service{DB: s.db, Sqids: sq, Bus: s.Bus}
	s.Projects = &projects.Service{DB: s.db, GH: ghAPI, Stack: s.Stack, Now: now}
	s.Runs = &runs.Service{DB: s.db, Stack: s.Stack, NC: s.nats.Conn, Now: now, Log: s.log,
		AnswerHandlers: map[string]runs.AnswerHandler{}, ToolHandlers: map[string]runs.ToolHandler{}, Bus: s.Bus}
	reviewer, err := s.reviewer()
	if err != nil {
		return err
	}
	var whSecret []byte
	if p := s.cfg.GitHub.ReviewerApp.WebhookSecret; p != "" {
		if b, err := os.ReadFile(s.cfg.Path(p)); err == nil {
			whSecret = []byte(strings.TrimSpace(string(b)))
		} else {
			s.log.Warn("github webhook secret unreadable; webhooks disabled", "err", err)
		}
	}
	s.PRs = &prs.Service{DB: s.db, Author: ghAPI, Reviewer: reviewer, Stack: s.Stack, Runs: s.Runs, WebhookSecret: whSecret, Now: now, Log: s.log, Bus: s.Bus}
	s.Runs.OnRunFinished = func(_ context.Context, runID int64) {
		go s.PRs.OnRunFinished(context.Background(), runID) // GitHub calls must not hold up event ingest
	}
	s.Runs.ToolHandlers["submit_review"] = s.PRs.SubmitReview
	s.Runs.AnswerHandlers["pr_review"] = s.PRs.AnswerPR

	stackSub, err := s.nats.Conn.Subscribe(natsd.StackWildcard, func(m *nats.Msg) {
		s.Runs.HandleStack(context.Background(), m)
	})
	if err != nil {
		return err
	}
	defer stackSub.Unsubscribe()
	for _, c := range []struct {
		name, filter string
		handle       func(context.Context, string, []byte) error
	}{
		{"controller-run-state", natsd.RunStateWildcard, s.Runs.HandleState},
		{"controller-run-logs", natsd.RunLogWildcard, s.Runs.HandleLog},
		{"controller-run-hooks", natsd.RunHookWildcard, s.Runs.HandleHook},
	} {
		cc, err := s.consume(ctx, c.name, c.filter, c.handle)
		if err != nil {
			return err
		}
		defer cc.Stop()
	}

	hbSub, err := s.nats.Conn.Subscribe(natsd.HeartbeatWildcard, func(m *nats.Msg) {
		if err := s.Fleet.Heartbeat(context.Background(), m.Subject, m.Data); err != nil {
			s.log.Warn("heartbeat", "subject", m.Subject, "err", err)
		}
	})
	if err != nil {
		return err
	}
	defer hbSub.Unsubscribe()

	// Template apply results: durable, so results published while the controller was
	// down are processed when it returns.
	tmplCons, err := s.js.CreateOrUpdateConsumer(ctx, StreamEvents, jetstream.ConsumerConfig{
		Durable: "controller-template-results", FilterSubject: natsd.TemplateEventWildcard,
		AckPolicy: jetstream.AckExplicitPolicy, MaxDeliver: 10, AckWait: 30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("template consumer: %w", err)
	}
	tmplCC, err := tmplCons.Consume(func(m jetstream.Msg) {
		if err := s.Fleet.TemplateResult(context.Background(), m.Subject(), m.Data()); err != nil {
			s.log.Error("template result", "subject", m.Subject(), "err", err)
			if errors.Is(err, fleet.ErrInvalid) {
				m.Term() // malformed: never redeliver
				return
			}
			m.NakWithDelay(5 * time.Second)
			return
		}
		m.Ack()
	})
	if err != nil {
		return err
	}
	defer tmplCC.Stop()

	// Background loops must stop before the deferred NATS/DB teardown runs, or a tick in
	// flight hits a closed database (found by the M3 e2e test under -race).
	loopCtx, stopLoop := context.WithCancel(ctx)
	var loops sync.WaitGroup
	defer func() { stopLoop(); loops.Wait() }()
	interval := s.LoopInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	schedInterval := s.SchedulerInterval
	if schedInterval == 0 {
		schedInterval = 10 * time.Second
	}
	sched := &scheduler.Scheduler{Fleet: s.Fleet, Now: now, Log: s.log, GH: ghAPI,
		Secret: func(name string) ([]byte, error) { return SecretGet(s.cfg.DataDir, name) }}
	reconcile := s.ReconcileInterval
	if reconcile == 0 {
		reconcile = 15 * time.Minute
	}
	loops.Add(4)
	go func() {
		defer loops.Done()
		s.PRs.Reconcile(loopCtx)
		t := time.NewTicker(reconcile)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				s.PRs.Reconcile(loopCtx)
			}
		}
	}()
	go func() { defer loops.Done(); s.Fleet.Loop(loopCtx, interval) }()
	go func() { defer loops.Done(); sched.Loop(loopCtx, schedInterval) }()
	go func() {
		defer loops.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				if err := s.Runs.SweepLost(loopCtx); err != nil && loopCtx.Err() == nil {
					s.log.Error("lost-run sweep", "err", err)
				}
			}
		}
	}()

	apiSrv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	enrollSrv := &http.Server{Handler: api.EnrollHandler(s.Fleet), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, len(listeners)+1)
	serve := func(srv *http.Server, l net.Listener) {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}
	for _, l := range listeners {
		go serve(apiSrv, l)
	}
	go serve(enrollSrv, enrollLn)

	s.mu.Lock()
	s.httpAddr, s.enrollAddr, s.natsURL = apiLn.Addr().String(), enrollLn.Addr().String(), s.nats.URL()
	s.mu.Unlock()
	s.log.Info("controller serving", "id", controllerID, "http", s.httpAddr, "unix", s.cfg.Listen.Unix,
		"enroll", s.enrollAddr, "nats", s.natsURL, "aws", prov != nil)
	close(s.ready)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	stopLoop()
	loops.Wait()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(err, apiSrv.Shutdown(shutCtx), enrollSrv.Shutdown(shutCtx))
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	// A previous unclean exit leaves the socket file behind; refuse only if something
	// is actually listening on it.
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%s: another controller is already listening", path)
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("unix listen: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil { // wrangl group may connect
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	api.Register(mux, api.Deps{Fleet: s.Fleet, Stack: s.Stack, Projects: s.Projects, Runs: s.Runs, PRs: s.PRs, JS: s.js, Bus: s.Bus})
	mux.HandleFunc("POST /gh/webhook", s.PRs.Webhook)
	return mux
}

// reviewer builds the reviewer App client from config (nil when not configured).
func (s *Server) reviewer() (gh.API, error) {
	if s.Reviewer != nil {
		return s.Reviewer, nil
	}
	app := s.cfg.GitHub.ReviewerApp
	if app.ID == 0 {
		s.log.Warn("github.reviewer_app not set: PRs are opened but not reviewed or merged by wrangl")
		return nil, nil
	}
	key, err := gh.LoadKey(s.cfg.Path(app.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("github reviewer app key: %w", err)
	}
	return &gh.Client{AppID: app.ID, Key: key}, nil
}

// github builds the author App client from config (nil when not configured).
func (s *Server) github() (gh.API, error) {
	if s.GH != nil {
		return s.GH, nil
	}
	app := s.cfg.GitHub.AuthorApp
	if app.ID == 0 {
		s.log.Warn("github.author_app not set: repo linking, issue sync and pushes to GitHub are disabled")
		return nil, nil
	}
	key, err := gh.LoadKey(s.cfg.Path(app.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("github author app key: %w", err)
	}
	return &gh.Client{AppID: app.ID, Key: key}, nil
}

// consume runs handle for every message on a durable WR_EVT consumer: malformed or
// forbidden messages are terminated, other errors redelivered.
func (s *Server) consume(ctx context.Context, durable, filter string, handle func(context.Context, string, []byte) error) (jetstream.ConsumeContext, error) {
	cons, err := s.js.CreateOrUpdateConsumer(ctx, StreamEvents, jetstream.ConsumerConfig{
		Durable: durable, FilterSubject: filter, AckPolicy: jetstream.AckExplicitPolicy,
		MaxDeliver: 10, AckWait: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("consumer %s: %w", durable, err)
	}
	return cons.Consume(func(m jetstream.Msg) {
		if err := handle(context.Background(), m.Subject(), m.Data()); err != nil {
			s.log.Error("event", "subject", m.Subject(), "err", err)
			if errors.Is(err, errs.Invalid) || errors.Is(err, errs.Forbidden) || errors.Is(err, errs.NotFound) {
				m.Term()
				return
			}
			m.NakWithDelay(5 * time.Second)
			return
		}
		m.Ack()
	})
}

// healthz reports each dependency; any failure returns 503.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	st := map[string]string{"db": "ok", "nats": "ok", "jetstream": "ok"}
	code := http.StatusOK
	fail := func(k string, err error) { st[k] = err.Error(); code = http.StatusServiceUnavailable }
	if err := s.db.R.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); err != nil {
		fail("db", err)
	}
	if !s.nats.Server.Running() || !s.nats.Conn.IsConnected() {
		fail("nats", errors.New("not connected"))
	}
	if _, err := s.js.AccountInfo(ctx); err != nil {
		fail("jetstream", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(st)
}
