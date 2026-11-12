package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentplugin"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Version is reported in heartbeats; set at build time with -ldflags.
var Version = "dev"

// DaemonOptions are test hooks; zero values give production behaviour.
type DaemonOptions struct {
	HeartbeatInterval time.Duration // default proto.HeartbeatInterval
	OrphanAfter       time.Duration // default proto.OrphanPoweroff
	Now               func() time.Time
	Poweroff          func() error // default: run Config.PoweroffCommand
	Log               *slog.Logger
	InstallMise       MiseInstaller // nil = never install mise (only `runner run` passes DefaultMiseInstaller)
	Upgrade           *SelfUpgrade  // nil = decline cmd.upgrade (only `runner run` enables it)
	Targets           TargetBackend // nil = real ssh/adb over SSM; tests pass a FakeTarget
}

// Connect opens the runner's NATS connection: TLS pinned to the controller CA, the
// runner's own credentials and inbox prefix, retrying forever.
func Connect(cfg *Config, extra ...nats.Option) (*nats.Conn, error) {
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := natsd.ClientTLS(ca)
	if err != nil {
		return nil, err
	}
	opts := append([]nats.Option{
		nats.UserCredentials(cfg.CredsFile),
		nats.Secure(tlsCfg),
		nats.CustomInboxPrefix(natsd.RunnerInboxPrefix(cfg.RunnerID)),
		nats.Name("wrangl-runner-" + cfg.Name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}, extra...)
	return nats.Connect(strings.Join(cfg.NATSURLs, ","), opts...)
}

// Run is the runner daemon. It returns when ctx ends, or after triggering poweroff.
func Run(ctx context.Context, cfg *Config, o DaemonOptions) error {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = proto.HeartbeatInterval
	}
	if o.OrphanAfter == 0 {
		o.OrphanAfter = proto.OrphanPoweroff
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	// The embedded skill, for the agent that runs here. Startup rather than an upgrade hook:
	// a runner self-upgrades by replacing this binary and restarting, so this covers every
	// upgrade without the upgrade path knowing about it. Ensure never fails.
	go agentplugin.Ensure(ctx, Version, o.Log)
	if o.Poweroff == nil {
		o.Poweroff = func() error {
			if cfg.Host || len(cfg.PoweroffCommand) == 0 {
				return errors.New("no poweroff_command configured (or a host runner)")
			}
			out, err := exec.Command(cfg.PoweroffCommand[0], cfg.PoweroffCommand[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%v: %s", err, out)
			}
			return nil
		}
	}

	var mu sync.Mutex
	lastConnected := o.Now()
	connected := false
	setConn := func(up bool) {
		mu.Lock()
		defer mu.Unlock()
		connected = up
		lastConnected = o.Now()
	}
	nc, err := Connect(cfg,
		nats.RetryOnFailedConnect(true),
		nats.ConnectHandler(func(*nats.Conn) { setConn(true); o.Log.Info("connected to controller") }),
		nats.ReconnectHandler(func(*nats.Conn) { setConn(true); o.Log.Info("reconnected to controller") }),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { setConn(false); o.Log.Warn("disconnected from controller", "err", err) }),
	)
	if err != nil {
		return err
	}
	defer nc.Close()
	if nc.IsConnected() {
		setConn(true)
	}

	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "ping"), func(m *nats.Msg) {
		b, _ := json.Marshal(proto.Ping{RunnerID: cfg.RunnerID, Version: Version})
		m.Respond(b)
	}); err != nil {
		return err
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	var applying, upgrading atomic.Bool
	var gate sync.Mutex // assign and upgrade decide under it: never a run starting mid-upgrade
	var binSHA atomic.Value
	binSHA.Store("")
	if o.Upgrade != nil {
		if sum, err := o.Upgrade.Sum(); err == nil {
			binSHA.Store(sum)
		} else {
			o.Log.Warn("self-upgrade off: cannot hash own binary", "err", err)
		}
	}
	worker := &Worker{Cfg: cfg, NC: nc, JS: js, Log: o.Log, Targets: o.Targets}
	defer worker.Wait() // let a cancelled run publish its final state before we disconnect
	applier := &Applier{Cfg: cfg, InstallMise: o.InstallMise}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "template"), func(m *nats.Msg) {
		var req proto.TemplateApply
		reply := func(a proto.TemplateAck) { b, _ := json.Marshal(a); m.Respond(b) }
		if err := json.Unmarshal(m.Data, &req); err != nil || req.Version <= 0 {
			reply(proto.TemplateAck{Reason: "bad request"})
			return
		}
		if upgrading.Load() {
			reply(proto.TemplateAck{Reason: "a self-upgrade is in progress"})
			return
		}
		if !applying.CompareAndSwap(false, true) {
			reply(proto.TemplateAck{Reason: "an apply is already running"})
			return
		}
		if len(worker.Active()) > 0 {
			applying.Store(false)
			reply(proto.TemplateAck{Reason: "a run is in progress"})
			return
		}
		reply(proto.TemplateAck{Accepted: true})
		go func() {
			defer applying.Store(false)
			o.Log.Info("applying template", "version", req.Version)
			logText, err := applier.Apply(ctx, req)
			res := proto.TemplateResult{Version: req.Version, OK: err == nil, Log: logText}
			if err != nil {
				res.Error = err.Error()
				o.Log.Error("template apply failed", "version", req.Version, "err", err)
			} else {
				o.Log.Info("template applied", "version", req.Version)
			}
			publishDurable(ctx, js, natsd.RunnerTemplateEventSubject(cfg.RunnerID), res, o.Log)
		}()
	}); err != nil {
		return err
	}

	respond := func(m *nats.Msg, v any) { b, _ := json.Marshal(v); m.Respond(b) }
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "upgrade"), func(m *nats.Msg) {
		var req proto.Upgrade
		if json.Unmarshal(m.Data, &req) != nil || !shaRe.MatchString(req.SHA256) {
			respond(m, proto.Ack{Reason: "bad request"})
			return
		}
		if o.Upgrade == nil || binSHA.Load().(string) == "" {
			respond(m, proto.Ack{Reason: "self-upgrade is off on this runner"})
			return
		}
		if req.SHA256 == binSHA.Load().(string) {
			respond(m, proto.Ack{Accepted: true, Reason: "already on that binary"})
			return
		}
		gate.Lock()
		if !upgrading.CompareAndSwap(false, true) {
			gate.Unlock()
			respond(m, proto.Ack{Reason: "already upgrading"})
			return
		}
		if len(worker.Active()) > 0 || applying.Load() {
			upgrading.Store(false)
			gate.Unlock()
			respond(m, proto.Ack{Reason: "busy"})
			return
		}
		gate.Unlock()
		respond(m, proto.Ack{Accepted: true})
		go func() {
			defer upgrading.Store(false)
			o.Log.Info("self-upgrade: downloading", "sha256", req.SHA256[:12])
			if err := o.Upgrade.Fetch(ctx, cfg.Controller, unameArch(), req.SHA256); err != nil {
				o.Log.Error("self-upgrade failed; staying on the current binary", "err", err)
				return
			}
			o.Log.Info("self-upgrade: binary replaced; restarting", "path", o.Upgrade.BinaryPath)
			if err := o.Upgrade.restart(); err != nil {
				// Still running the old code: keep reporting the old hash, so the
				// controller retries (and the run claim stays blocked) rather than
				// believing an upgrade that has not happened.
				o.Log.Error("self-upgrade: restart failed; the new binary runs at the next restart", "err", err)
				return
			}
			binSHA.Store(req.SHA256) // only reached when Restart is replaced (tests); exec never returns
		}()
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "assign"), func(m *nats.Msg) {
		var a proto.Assignment
		if err := json.Unmarshal(m.Data, &a); err != nil || a.RunID <= 0 {
			respond(m, proto.Ack{Reason: "bad assignment"})
			return
		}
		if applying.Load() {
			respond(m, proto.Ack{Reason: "applying a template"})
			return
		}
		gate.Lock()
		if upgrading.Load() {
			gate.Unlock()
			respond(m, proto.Ack{Reason: "upgrading the runner binary"})
			return
		}
		err := worker.Start(ctx, a)
		gate.Unlock()
		if err != nil {
			respond(m, proto.Ack{Reason: err.Error()})
			return
		}
		o.Log.Info("run assigned", "run", a.RunID, "task", a.TaskSqid, "repo", a.Repo)
		respond(m, proto.Ack{Accepted: true})
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "cancel"), func(m *nats.Msg) {
		var ref proto.RunRef
		json.Unmarshal(m.Data, &ref)
		if !worker.Cancel(ref.RunID) {
			respond(m, proto.Ack{Reason: fmt.Sprintf("run %d is not running here", ref.RunID)})
			return
		}
		respond(m, proto.Ack{Accepted: true})
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "answer"), func(m *nats.Msg) {
		var a proto.Answer
		if json.Unmarshal(m.Data, &a) == nil && !worker.DeliverAnswer(a) {
			o.Log.Warn("answer for a question nobody is waiting on", "question", a.QuestionID)
		}
	}); err != nil {
		return err
	}
	go func() {
		if err := worker.ServeAgentSocket(ctx, cfg.Socket()); err != nil {
			o.Log.Error("agent socket", "path", cfg.Socket(), "err", err)
		}
	}()
	// Web terminal (owner-facing): list, type into, resize and open/close PTYs. Input and
	// resize are plain publishes; the controller forwards them only while the owner has
	// taken control.
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "term.in.*"), func(m *nats.Msg) {
		if err := worker.TermInput(termID(m.Subject), m.Data); err != nil {
			o.Log.Debug("term input", "err", err)
		}
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "term.size.*"), func(m *nats.Msg) {
		var sz proto.TermSize
		if json.Unmarshal(m.Data, &sz) == nil {
			if err := worker.TermResize(termID(m.Subject), sz); err != nil {
				o.Log.Debug("term resize", "err", err)
			}
		}
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "term.list"), func(m *nats.Msg) {
		respond(m, worker.TermList())
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "shell.open"), func(m *nats.Msg) {
		id, err := worker.OpenShell(ctx)
		if err != nil {
			respond(m, proto.TermOpenReply{Error: err.Error()})
			return
		}
		respond(m, proto.TermOpenReply{ID: id})
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, "term.close.*"), func(m *nats.Msg) {
		if err := worker.CloseTerm(termID(m.Subject)); err != nil {
			respond(m, proto.TermOpenReply{Error: err.Error()})
			return
		}
		respond(m, proto.TermOpenReply{})
	}); err != nil {
		return err
	}
	for _, kind := range []string{"diff", "tree", "file"} {
		if _, err := nc.Subscribe(natsd.RunnerCmdSubject(cfg.RunnerID, kind), func(m *nats.Msg) {
			var ref proto.RunRef
			json.Unmarshal(m.Data, &ref)
			respond(m, worker.Inspect(ctx, kind, ref))
		}); err != nil {
			return err
		}
	}

	beat := func() {
		if !nc.IsConnected() {
			return
		}
		hb := SampleHeartbeat(ctx, Version)
		hb.TemplateVersion = AppliedVersion(cfg)
		hb.Applying = applying.Load()
		active := worker.Active()
		hb.Sessions = len(active)
		if len(active) > 0 {
			hb.RunID = active[0] // for controllers from before Sessions
		}
		hb.BinarySHA = binSHA.Load().(string)
		hb.Upgrading = upgrading.Load()
		hb.Claude = ScanClaude("/proc", time.Now())
		b, _ := json.Marshal(hb)
		if err := nc.Publish(natsd.RunnerHeartbeatSubject(cfg.RunnerID), b); err != nil {
			o.Log.Warn("heartbeat", "err", err)
		}
	}
	beat()
	t := time.NewTicker(o.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		beat()
		mu.Lock()
		orphaned := !connected && o.Now().Sub(lastConnected) >= o.OrphanAfter
		since := lastConnected
		mu.Unlock()
		if orphaned {
			o.Log.Error("controller unreachable; powering off (instance terminates on shutdown)", "since", since)
			if err := o.Poweroff(); err != nil {
				o.Log.Error("poweroff failed", "err", err)
				continue // keep trying every tick
			}
			return nil
		}
	}
}

// publishDurable publishes v to JetStream, retrying until the server acks or ctx ends,
// so an apply result survives a controller restart.
func publishDurable(ctx context.Context, js jetstream.JetStream, subject string, v any, log *slog.Logger) {
	b, _ := json.Marshal(v)
	for attempt := 0; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := js.Publish(pctx, subject, b)
		cancel()
		if err == nil {
			return
		}
		log.Warn("publish failed; retrying", "subject", subject, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(min(attempt+1, 10)) * time.Second):
		}
	}
}

// Status connects once and measures a round trip to the controller.
func Status(ctx context.Context, cfg *Config) (time.Duration, error) {
	nc, err := Connect(cfg, nats.MaxReconnects(0), nats.Timeout(5*time.Second))
	if err != nil {
		return 0, err
	}
	defer nc.Close()
	return nc.RTT()
}

// termID is the session id in a wr.r.<r>.cmd.term.<op>.<id> subject.
func termID(subject string) string {
	return subject[strings.LastIndex(subject, ".")+1:]
}
