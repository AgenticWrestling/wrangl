package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/agents"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/costs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentplugin"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/cfaccess"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/deliverables"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/ghsetup"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuetools"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/plans"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/prs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/scheduler"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/taskreview"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/webpush"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/mcp"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stacktools"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/web"
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
	GH                  gh.API                 // overrides the GitHub App client built from the accounts
	Setup               *ghsetup.Service
	multi               *gh.Multi
	Reviewer            gh.API        // overrides the reviewer App client built from config
	ReconcileInterval   time.Duration // PR reconcile poll (default 15m)
	Now                 func() time.Time
	LoopInterval        time.Duration // offline sweep + reaper cadence (default 30s)
	SchedulerInterval   time.Duration // scale-up cadence (default 10s)
	AllowLoopbackEnroll bool
	// ProbeTarget replaces the remote-target health probe (tests); nil = EC2 state check.
	ProbeTarget func(ctx context.Context, g dbgen.RemoteTarget, conn proto.Connection) error

	mu         sync.Mutex
	httpAddr   string
	enrollAddr string
	natsURL    string

	db           *db.DB
	nats         *natsd.Embedded
	js           jetstream.JetStream
	Fleet        *fleet.Service
	Stack        *stack.Service
	Projects     *projects.Service
	Runs         *runs.Service
	PRs          *prs.Service
	Plans        *plans.Service
	Agents       *agents.Service // controller agents ("Ask?" on a task)
	Deliverables *deliverables.Service
	TaskReview   *taskreview.Service
	Costs        *costs.Service // nil without AWS
	Push         *webpush.Service
	Bus          *events.Bus
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
	s.Costs = newCosts(ac, s.cfg.AWS.Region, s.log)
	return &provision.Provisioner{EC2: ec2.NewFromConfig(ac), SSM: ssm.NewFromConfig(ac), ControllerID: controllerID}, nil
}

// newCosts builds the spend/inventory service from the same credentials. Cost Explorer and the
// Pricing API are global services served from us-east-1; instances are read in every enabled
// region.
func newCosts(ac aws.Config, home string, log *slog.Logger) *costs.Service {
	in := func(region string) aws.Config { c := ac.Copy(); c.Region = region; return c }
	return &costs.Service{
		Pricing: pricing.NewFromConfig(in("us-east-1")),
		Cost:    costexplorer.NewFromConfig(in("us-east-1")),
		EC2:     func(region string) costs.EC2API { return ec2.NewFromConfig(in(region)) },
		Regions: func(ctx context.Context) ([]string, error) {
			out, err := ec2.NewFromConfig(ac).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
			if err != nil {
				return nil, err
			}
			var rs []string
			for _, r := range out.Regions {
				rs = append(rs, aws.ToString(r.RegionName))
			}
			return rs, nil
		},
		Home: home, Log: log,
	}
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
		ProbeTarget: s.ProbeTarget,
	}

	ghAPI, err := s.github(ctx)
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
	s.Stack = &stack.Service{DB: s.db, Sqids: sq, Bus: s.Bus, Now: now}
	s.Projects = &projects.Service{DB: s.db, GH: ghAPI, Stack: s.Stack, Now: now}
	s.Runs = &runs.Service{DB: s.db, Stack: s.Stack, NC: s.nats.Conn, Now: now, Log: s.log,
		AnswerHandlers: map[string]runs.AnswerHandler{}, ToolHandlers: map[string]runs.ToolHandler{}, ApprovalHandlers: map[string]func(context.Context, json.RawMessage) error{}, Bus: s.Bus}
	reviewer, hasReviewer, err := s.reviewer()
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
	s.PRs = &prs.Service{DB: s.db, Author: ghAPI, Reviewer: reviewer, HasReviewer: hasReviewer, WebhookSecrets: s.Setup.WebhookSecrets, Stack: s.Stack, Runs: s.Runs, WebhookSecret: whSecret, Now: now, Log: s.log, Bus: s.Bus}
	vapid, err := webpush.EnsureKeys(SecretsDir(s.cfg.DataDir))
	if err != nil {
		return fmt.Errorf("VAPID keys: %w", err)
	}
	subject := "mailto:" + s.cfg.Cloudflare.OwnerEmail
	if s.cfg.Cloudflare.OwnerEmail == "" {
		subject = "https://github.com/agenticwrestling/wrangl"
	}
	s.Push = &webpush.Service{DB: s.db, Keys: vapid, Subject: subject, Log: s.log}
	s.Runs.OnQuestion = func(q dbgen.Question) {
		title := map[string]string{"agent": "An agent has a question", "approval": "Approval needed", "pr_review": "PR waiting for review",
			"plan_review": "Proposed split ready", "deliverable_review": "Deliverables ready for review", "task_review": "A task needs your review", "system": "A run needs you"}[q.Kind]
		s.Push.Notify(webpush.Notification{Title: title, Body: q.Prompt, URL: fmt.Sprintf("/inbox#q%d", q.ID), Tag: fmt.Sprintf("q%d", q.ID)})
	}
	s.Fleet.OnOffline = func(name string) {
		s.Push.Notify(webpush.Notification{Title: "Runner offline", Body: name + " stopped sending heartbeats", URL: "/runners", Tag: "runner-" + name})
	}
	s.Runs.OnRunFinished = func(ctx context.Context, runID int64) {
		if r, err := s.db.RQ.GetRun(ctx, runID); err == nil && (r.State == "failed" || r.State == "blocked") {
			s.Push.Notify(webpush.Notification{Title: "Run " + r.State, Body: fmt.Sprintf("run %d: %s", runID, r.Error+r.StatusText), URL: fmt.Sprintf("/run/%d", runID), Tag: fmt.Sprintf("run%d", runID)})
		}
		go s.PRs.OnRunFinished(context.Background(), runID) // GitHub calls must not hold up event ingest
		if s.Deliverables != nil {
			go s.Deliverables.OnRunFinished(context.Background(), s.review(), runID)
		}
	}
	s.Runs.OnAgentNotice = func(r dbgen.Run, message string) {
		if message == "" {
			message = "The agent is waiting for you"
		}
		s.Push.Notify(webpush.Notification{Title: "Agent needs attention", Body: message, URL: fmt.Sprintf("/run/%d", r.ID), Tag: fmt.Sprintf("notice-run%d", r.ID)})
	}
	issueTools := &issuetools.Service{DB: s.db, GH: ghAPI, Now: now, Runs: s.Runs, Log: s.log}
	s.Runs.ToolHandlers["issue_comment"] = issueTools.Comment
	s.Runs.ToolHandlers["issue_create"] = issueTools.Create
	s.Runs.ToolHandlers["issue_edit"] = issueTools.Edit
	s.Runs.ToolHandlers["issue_close"] = issueTools.Close
	s.Runs.ToolHandlers["issue_link_task"] = issueTools.LinkTask
	s.Runs.ApprovalHandlers["issue_close"] = issueTools.CloseApproved
	s.Runs.ToolHandlers["issue_search"] = issueTools.Search
	s.Runs.ToolHandlers["issue_get"] = issueTools.Get
	s.Runs.ToolHandlers["issue_comments"] = issueTools.Comments
	s.Runs.ToolHandlers["submit_review"] = s.PRs.SubmitReview
	s.Runs.ToolHandlers["submit_citation_audit"] = s.PRs.SubmitCitationAudit
	s.Deliverables = &deliverables.Service{DB: s.db, Dir: filepath.Join(s.cfg.DataDir, "deliverables"), Bus: s.Bus, Log: s.log}
	s.Runs.ToolHandlers["submit_deliverable"] = s.Deliverables.Submit
	s.Runs.AnswerHandlers["deliverable_review"] = s.Deliverables.Answer(s.review())
	s.TaskReview = &taskreview.Service{DB: s.db, Runs: s.Runs, Stack: s.Stack, Bus: s.Bus, Log: s.log, Now: now}
	s.Runs.AnswerHandlers["task_review"] = s.TaskReview.Answer
	s.Plans = &plans.Service{DB: s.db, Stack: s.Stack, Runs: s.Runs, Log: s.log}
	s.Agents = &agents.Service{DB: s.db, Stack: s.Stack, Bus: s.Bus, Log: s.log, Secret: func(name string) ([]byte, error) { return SecretGet(s.cfg.DataDir, name) }}
	s.Agents.Recover(context.Background())
	s.Agents.Queue = func(ctx context.Context, sqid string) error { _, err := s.Plans.QueueWork(ctx, sqid); return err }
	s.Agents.Dequeue = func(ctx context.Context, sqid string) error { _, err := s.Runs.Dequeue(ctx, sqid); return err }
	s.Runs.ToolHandlers["propose_split"] = s.Plans.ProposeSplit
	s.Runs.AnswerHandlers["plan_review"] = s.Plans.Answer
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
	needApp := func(owner, repo string, cause error) {
		s.Push.Notify(webpush.Notification{Title: "GitHub App needed for " + owner, Body: "Work on " + owner + "/" + repo + " is waiting: " + cause.Error(),
			URL: "/setup?account=" + url.QueryEscape(owner) + "&repo=" + url.QueryEscape(owner+"/"+repo), Tag: "ghapp-" + strings.ToLower(owner)})
	}
	launchStuck := func(template string, failed int64) {
		s.Push.Notify(webpush.Notification{Title: "Runners are not enrolling", Body: fmt.Sprintf("%d %s runners in a row never enrolled; launching is paused for an hour. Check the template's image, user-data and network.", failed, template),
			URL: "/runners", Tag: "enrol-" + template})
	}
	sched := &scheduler.Scheduler{OnNeedApp: needApp, OnLaunchStuck: launchStuck, Fleet: s.Fleet, Now: now, Log: s.log, GH: ghAPI,
		Secret: func(name string) ([]byte, error) { return SecretGet(s.cfg.DataDir, name) }}
	if s.Agents != nil {
		s.Agents.Launch = sched.LaunchFor
	}
	s.Fleet.OnTargetLost = s.Runs.FailRun
	reconcile := s.ReconcileInterval
	if reconcile == 0 {
		reconcile = 15 * time.Minute
	}
	loops.Add(7)
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
	go func() { defer loops.Done(); s.Deliverables.Loop(loopCtx) }()
	go func() { defer loops.Done(); s.TaskReview.Loop(loopCtx, 30*time.Second) }()
	go func() {
		defer loops.Done()
		if s.Costs == nil {
			return
		}
		s.Costs.Loop(loopCtx, time.Minute)
	}()
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
				if err := s.Runs.SweepStuck(loopCtx); err != nil && loopCtx.Err() == nil {
					s.log.Error("stuck-run sweep", "err", err)
				}
			}
		}
	}()

	routes := s.routes()
	// Long-lived streams (live UI, MCP, event feeds, terminals) end when connCtx is cancelled at
	// shutdown; otherwise Shutdown waits for them until its timeout.
	connCtx, stopConns := context.WithCancel(context.Background())
	defer stopConns()
	baseCtx := func(net.Listener) context.Context { return connCtx }
	apiSrv := &http.Server{Handler: routes, ReadHeaderTimeout: 10 * time.Second, BaseContext: baseCtx} // unix socket: trusted by file permissions
	tcpHandler := routes
	if cf := s.cfg.Cloudflare; cf.Team != "" {
		v := &cfaccess.Verifier{Team: cf.Team, AUD: cf.AUD, Owner: cf.OwnerEmail}
		tcpHandler = v.Middleware(routes, func(r *http.Request) bool {
			return r.URL.Path == "/gh/webhook" || r.URL.Path == "/healthz" // HMAC-verified / no data
		})
	} else {
		s.log.Warn("cloudflare.team not set: the TCP API is not verified (keep listen.http on loopback)")
	}
	tcpSrv := &http.Server{Handler: tcpHandler, ReadHeaderTimeout: 10 * time.Second, BaseContext: baseCtx}
	enrollSrv := &http.Server{Handler: api.EnrollHandler(s.Fleet), ReadHeaderTimeout: 10 * time.Second, BaseContext: baseCtx}
	errc := make(chan error, len(listeners)+1)
	serve := func(srv *http.Server, l net.Listener) {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}
	go serve(tcpSrv, apiLn)
	for _, l := range listeners[1:] {
		go serve(apiSrv, l)
	}
	go serve(enrollSrv, enrollLn)

	s.mu.Lock()
	s.httpAddr, s.enrollAddr, s.natsURL = apiLn.Addr().String(), enrollLn.Addr().String(), s.nats.URL()
	s.mu.Unlock()
	s.log.Info("controller serving", "id", controllerID, "http", s.httpAddr, "unix", s.cfg.Listen.Unix,
		"enroll", s.enrollAddr, "nats", s.natsURL, "aws", prov != nil)
	// The embedded skill, refreshed for this binary's version. Startup rather than a dedicated
	// upgrade hook: every upgrade path -- controller.sh upgrade, a package, a hand-copied
	// binary -- restarts the process, so this is the one place that cannot be bypassed. It is
	// after `ready` is closed and never returns an error, because a developer's Claude
	// installation is not a dependency of the controller.
	go agentplugin.Ensure(ctx, runner.Version, s.log)
	close(s.ready)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	stopLoop()
	loops.Wait()
	stopConns()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Graceful first; whatever is still open after the grace period (a client's spare,
	// never-used connection counts as active for 5 s) is closed.
	stop := func(name string, srv *http.Server) error {
		if err := srv.Shutdown(shutCtx); err != nil {
			s.log.Warn("listener did not drain in time; closing it", "listener", name, "err", err)
			return srv.Close()
		}
		return nil
	}
	return errors.Join(err, stop("tcp", tcpSrv), stop("unix", apiSrv), stop("enrol", enrollSrv))
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
	term := &api.TermHub{}
	s.Fleet.Attached = term.Attached
	deps := api.Deps{Fleet: s.Fleet, Stack: s.Stack, Projects: s.Projects, Runs: s.Runs, PRs: s.PRs, Plans: s.Plans, Push: s.Push, JS: s.js, Bus: s.Bus, Term: term, Deliverables: s.Deliverables, Costs: s.Costs, Agents: s.Agents,
		Setup: s.setupService(), Secrets: &api.SecretStore{
			Set:  func(name string, r io.Reader) error { return SecretSet(s.cfg.DataDir, name, r) },
			List: func() ([]string, error) { return SecretList(s.cfg.DataDir) },
			Has:  func(name string) bool { _, err := SecretGet(s.cfg.DataDir, name); return err == nil },
		}}
	api.Register(mux, deps)
	mux.HandleFunc("POST /gh/webhook", s.PRs.Webhook)
	ui, err := web.New(deps)
	if err != nil { // embedded templates: a parse error is a build bug, caught by tests
		panic(fmt.Sprintf("web templates: %v", err))
	}
	ui.Register(mux)
	// M10: the owner's own agents get the work-stack tools over MCP (Streamable HTTP).
	// Same exposure as /api/v1: the unix socket, and TCP behind Cloudflare Access.
	remote := &mcp.Server{Name: "wrangl", Version: runner.Version, Tools: stacktools.For("user"),
		Call: func(ctx context.Context, name string, args json.RawMessage) (any, error) {
			return s.Runs.ExecTool(ctx, runs.UserScope(), name, args)
		}}
	mux.Handle("/mcp", remote.HTTPHandler())
	return mux
}

// reviewer builds the reviewer client. Reviewer Apps are per account (created from Settings);
// an App set in controller.yaml (github.reviewer_app) serves owners with none of their own.
// has reports whether an owner can be reviewed; it is nil when a test supplies its own client.
func (s *Server) reviewer() (api gh.API, has func(owner string) bool, err error) {
	if s.Reviewer != nil {
		return s.Reviewer, nil, nil
	}
	app := s.cfg.GitHub.ReviewerApp
	legacy := func(string) (gh.AppRef, bool) { return gh.AppRef{}, false }
	if app.ID != 0 {
		keyPath := s.cfg.Path(app.PrivateKey)
		if _, kerr := gh.LoadKey(keyPath); kerr != nil {
			return nil, nil, fmt.Errorf("github reviewer app key: %w", kerr)
		}
		legacy = func(string) (gh.AppRef, bool) { return gh.AppRef{AppID: app.ID, KeyPath: keyPath}, true }
	}
	m := &gh.Multi{Role: "reviewer", Lookup: func(owner string) (gh.AppRef, bool) {
		if ref, ok := s.Setup.LookupReviewer(owner); ok {
			return ref, true
		}
		return legacy(owner)
	}}
	prev := s.Setup.Changed
	s.Setup.Changed = func() {
		if prev != nil {
			prev()
		}
		m.Forget()
	}
	return m, m.Has, nil
}

// github builds the author App client from config (nil when not configured).
func (s *Server) github(ctx context.Context) (gh.API, error) {
	s.Setup = s.setupService()
	if s.GH != nil {
		return s.GH, nil
	}
	s.multi = &gh.Multi{Lookup: s.Setup.Lookup}
	s.Setup.Changed = s.multi.Forget
	// A PAT, if one is configured, for owners with no App. Read once at startup: the file is a
	// credential, so a read per request would widen the window in which it is on disk unguarded,
	// and rotating it is a restart either way.
	if p := s.cfg.GitHub.PATFile; p != "" {
		b, err := os.ReadFile(s.cfg.Path(p))
		switch {
		case err != nil:
			// Not fatal: an unreadable PAT must not stop a controller whose other accounts have
			// Apps, and the NoAppError a caller then meets already names the remedy.
			s.log.Warn("github pat_file unreadable; PAT mode disabled", "path", s.cfg.Path(p), "err", err)
		default:
			s.multi.PAT = strings.TrimSpace(string(b))
			if s.multi.PAT == "" {
				s.log.Warn("github pat_file is empty; PAT mode disabled", "path", s.cfg.Path(p))
			} else {
				s.log.Info("github PAT mode enabled for owners with no App", "path", s.cfg.Path(p))
			}
		}
	}
	// The single App of earlier versions (this Setup page's, or github.author_app in
	// controller.yaml) becomes the account that owns it.
	app := s.cfg.GitHub.AuthorApp
	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.Setup.MigrateLegacy(mctx, app.ID, s.cfg.Path(app.PrivateKey)); err != nil {
		s.log.Warn("could not adopt the existing GitHub App into an account; add the account in Settings", "err", err)
	}
	if accts, _ := s.Setup.Accounts(ctx); len(accts) == 0 {
		s.log.Warn("no GitHub account is set up: add yours in Settings to link repositories and push")
	}
	return s.multi, nil
}

// setupService is the GitHub account/App setup service (also used by the web Settings pages).
func (s *Server) setupService() *ghsetup.Service {
	if s.Setup != nil {
		return s.Setup
	}
	return &ghsetup.Service{DB: s.db, DataDir: s.cfg.DataDir, PublicURL: strings.TrimSuffix(s.cfg.Web.PublicURL, "/")}
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

// review is what the deliverable-review flow needs.
func (s *Server) review() deliverables.Review {
	return deliverables.Review{Runs: s.Runs, Stack: s.Stack}
}
