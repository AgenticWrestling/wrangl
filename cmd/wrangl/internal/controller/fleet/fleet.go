// Package fleet is the controller's runner lifecycle: templates, enrolment tokens,
// launch, enrolment, heartbeats, offline detection and the idle reaper.
package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Errors the API maps to HTTP status codes.
var (
	ErrNotFound  = errs.NotFound
	ErrInvalid   = errs.Invalid
	ErrForbidden = errs.Forbidden
	ErrConflict  = errs.Conflict
	ErrNoAWS     = errors.New("AWS is not configured on this controller (set aws.region, aws.subnet_ids, aws.security_group_id)")
)

// Timing.
const (
	EnrollDeadline = 15 * time.Minute // launched runner must enrol within this
	TokenTTL       = 30 * time.Minute
	// RAM/disk are measured on the box, which reports less than the nominal size
	// (kernel reservations, filesystem overhead). Accept within this fraction.
	measureTolerance = 0.90
)

// Service holds the fleet's dependencies. Zero-value fields are filled by the server.
type Service struct {
	DB                               *db.DB
	Keys                             *natsd.Keys
	NC                               *nats.Conn // controller connection, for commands to runners
	NATSCA                           []byte
	NATSURLs                         []string
	Prov                             *provision.Provisioner // nil when AWS is not configured
	ControllerID                     string
	EnrollURL                        string // http://<private-ip>:4223, written into user-data
	SubnetID                         string
	SecurityGroupID, InstanceProfile string
	BinDir                           string // <data-dir>/bin
	sumCache                         sync.Map
	Now                              func() time.Time
	Log                              *slog.Logger
	Bus                              *events.Bus               // change notices
	OnOffline                        func(runner string)       // web push (M8)
	Attached                         func(runnerID int64) bool // someone has a web terminal open: not idle
	hist                             history                   // recent heartbeats for sparklines
	// AllowLoopbackEnroll lets tests launch with a 127.0.0.1 enrol URL. Production
	// refuses: a runner on another host could never reach it.
	AllowLoopbackEnroll bool
	// ProbeTarget overrides the remote-target health probe (tests); nil = EC2 state + direct dial.
	ProbeTarget func(ctx context.Context, g dbgen.RemoteTarget, conn proto.Connection) error
	// OnTargetLost fails a run whose remote target is gone (runs.Service.FailRun).
	OnTargetLost func(ctx context.Context, runID int64, reason string) error
}

func (s *Service) now() time.Time { return s.Now().UTC() }

// note appends to the Logs page. Failures are logged, never returned: a missing log line
// must not fail the operation it describes.
func (s *Service) note(ctx context.Context, kind, level, subject, title, detail string) {
	err := s.DB.Q.AddLogEntry(context.WithoutCancel(ctx), dbgen.AddLogEntryParams{
		At: ts(s.now()), Kind: kind, Level: level, Subject: subject, Title: title, Detail: detail,
	})
	if err != nil {
		s.Log.Error("log entry", "err", err)
		return
	}
	if s.Bus != nil {
		s.Bus.Publish("log", subject)
	}
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

// --- templates ----------------------------------------------------------------

// CreateTemplate parses and stores a template file.
func (s *Service) CreateTemplate(ctx context.Context, yamlBody []byte) (dbgen.RunnerTemplate, error) {
	p, err := ParseTemplate(yamlBody)
	if err != nil {
		return dbgen.RunnerTemplate{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	t, err := s.DB.Q.CreateTemplate(ctx, p)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return t, fmt.Errorf("%w: template %q already exists", ErrConflict, p.Name)
	}
	if err == nil {
		err = s.syncAgents(ctx, t, yamlBody)
	}
	return t, err
}

// syncAgents applies a template file's allow-list ("agents:"), or wraps an embedded agent block
// as an agent definition and allows it.
func (s *Service) syncAgents(ctx context.Context, t dbgen.RunnerTemplate, yamlBody []byte) error {
	if err := s.syncMode(ctx, t, yamlBody); err != nil {
		return err
	}
	if names := TemplateAgents(yamlBody); len(names) > 0 {
		if err := agentdef.SetTemplateAgents(ctx, s.DB.Q, t.ID, names); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return nil
	}
	return agentdef.EnsureForTemplate(ctx, s.DB.Q, t.ID, t.Name, t.AgentJson)
}

// Template returns a template by name.
func (s *Service) Template(ctx context.Context, name string) (dbgen.RunnerTemplate, error) {
	t, err := s.DB.RQ.GetTemplateByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	return t, err
}

// DeleteTemplate removes a template that no runner, task or project references.
func (s *Service) DeleteTemplate(ctx context.Context, name string) error {
	n, err := s.DB.Q.DeleteTemplate(ctx, name)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return fmt.Errorf("%w: template %q is still in use", ErrConflict, name)
		}
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	return nil
}

// --- tokens -----------------------------------------------------------------------

func newToken() (plain string, hash []byte) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	plain = "wre_" + base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(plain))
	return plain, h[:]
}

// MintToken creates a one-time token for adopting a box the controller did not launch.
func (s *Service) MintToken(ctx context.Context, template string, ttl time.Duration) (string, time.Time, error) {
	t, err := s.Template(ctx, template)
	if err != nil {
		return "", time.Time{}, err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return "", time.Time{}, fmt.Errorf("%w: ttl must be between 0 and 24h", ErrInvalid)
	}
	plain, hash := newToken()
	exp := s.now().Add(ttl)
	_, err = s.DB.Q.CreateEnrollmentToken(ctx, dbgen.CreateEnrollmentTokenParams{
		TokenHash: hash, TemplateID: t.ID, ExpiresAt: ts(exp),
	})
	return plain, exp, err
}

// --- launch / terminate ------------------------------------------------------------

// binarySums returns uname-m -> sha256 for each runner binary available to serve. Hashes
// are cached by path, size and mtime (heartbeats ask on every beat).
func (s *Service) binarySums() (map[string]string, error) {
	sums := map[string]string{}
	for _, arch := range []string{"aarch64", "x86_64"} {
		path, ok := s.BinaryPath(arch)
		if !ok {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
		if v, ok := s.sumCache.Load(key); ok {
			sums[arch] = v.(string)
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		sums[arch] = hex.EncodeToString(h.Sum(nil))
		s.sumCache.Store(key, sums[arch])
	}
	return sums, nil
}

// BinaryPath finds the runner binary for a `uname -m` arch: <bin-dir>/wrangl-linux-<arch>,
// else this controller's own executable when it is a linux binary for that arch.
func (s *Service) BinaryPath(arch string) (string, bool) {
	p := filepath.Join(s.BinDir, "wrangl-linux-"+arch)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, true
	}
	own := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if runtime.GOOS == "linux" && own == arch {
		if exe, err := os.Executable(); err == nil {
			return exe, true
		}
	}
	return "", false
}

func unameArch(ec2Arch string) string {
	if ec2Arch == "arm64" {
		return "aarch64"
	}
	return ec2Arch
}

// Launch starts one runner on a new EC2 instance.
func (s *Service) Launch(ctx context.Context, template string) (dbgen.Runner, error) {
	var zero dbgen.Runner
	if s.Prov == nil {
		return zero, ErrNoAWS
	}
	t, err := s.Template(ctx, template)
	if err != nil {
		return zero, err
	}
	if host, _, err := net.SplitHostPort(strings.TrimPrefix(s.EnrollURL, "http://")); err != nil || net.ParseIP(host) == nil || (net.ParseIP(host).IsLoopback() && !s.AllowLoopbackEnroll) {
		return zero, fmt.Errorf("%w: listen.enroll must be the controller's private VPC IP so runners can reach it (got %q)", ErrInvalid, s.EnrollURL)
	}
	sums, err := s.binarySums()
	if err != nil {
		return zero, err
	}
	if sums[unameArch(t.Arch)] == "" {
		return zero, fmt.Errorf("%w: no %s runner binary: copy build/wrangl-linux-%s to %s", ErrInvalid, t.Arch, unameArch(t.Arch), s.BinDir)
	}
	var types []string
	json.Unmarshal([]byte(t.InstanceTypes), &types)

	name := s.newName(ctx)
	plain, hash := newToken()
	tokID, err := s.DB.Q.CreateEnrollmentToken(ctx, dbgen.CreateEnrollmentTokenParams{
		TokenHash: hash, TemplateID: t.ID, RunnerName: name, ExpiresAt: ts(s.now().Add(EnrollDeadline)),
	})
	if err != nil {
		return zero, err
	}
	ud, err := provision.RenderUserData(provision.UserDataParams{EnrollURL: s.EnrollURL, Token: plain, SHA256: sums})
	if err != nil {
		return zero, err
	}
	inst, err := s.Prov.Launch(ctx, provision.Spec{
		Runner: name, Template: t.Name, Arch: t.Arch,
		MinVCPU: int(t.MinVcpu), MinRAMMiB: int(t.MinRamMib), DiskGiB: int(t.MinDiskGib),
		InstanceTypes: types, AMI: t.Ami, SubnetID: s.SubnetID, SecurityGroupID: s.SecurityGroupID,
		InstanceProfile: s.InstanceProfile, UserData: ud,
	})
	if err != nil {
		return zero, err
	}
	var r dbgen.Runner
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		var err error
		r, err = q.CreateLaunchedRunner(ctx, dbgen.CreateLaunchedRunnerParams{
			Name: name, TemplateID: t.ID, InstanceID: nullStr(inst.ID), InstanceType: inst.Type,
			Az: inst.AZ, Arch: t.Arch, PrivateIp: inst.PrivateIP, LaunchedAt: nullStr(ts(s.now())),
		})
		if err != nil {
			return err
		}
		return q.BindTokenToRunner(ctx, dbgen.BindTokenToRunnerParams{
			InstanceID: nullStr(inst.ID), RunnerID: sql.NullInt64{Int64: r.ID, Valid: true}, ID: tokID,
		})
	})
	if err != nil {
		// The instance exists but we failed to record it: terminate rather than orphan it.
		s.Log.Error("recording launched runner failed; terminating instance", "instance", inst.ID, "err", err)
		_ = s.Prov.Terminate(context.WithoutCancel(ctx), inst.ID)
		return zero, err
	}
	s.Log.Info("launched runner", "runner", name, "instance", inst.ID, "type", inst.Type)
	s.note(ctx, "runner", "info", name, "Runner launched", fmt.Sprintf("%s · %s · template %s", inst.Type, inst.ID, t.Name))
	s.Bus.Publish("runner", name)
	return r, nil
}

// Terminate stops a controller-launched runner now.
func (s *Service) Terminate(ctx context.Context, name string) error {
	r, err := s.DB.RQ.GetRunnerByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: runner %q", ErrNotFound, name)
	} else if err != nil {
		return err
	}
	if r.State == "terminated" {
		return nil
	}
	if r.Launched == 0 || !r.InstanceID.Valid {
		return fmt.Errorf("%w: %s was adopted, not launched by this controller; stop the box yourself", ErrForbidden, name)
	}
	return s.terminate(ctx, r.ID, r.Name, r.InstanceID.String, "requested")
}

func (s *Service) terminate(ctx context.Context, id int64, name, instanceID, reason string) error {
	if s.Prov == nil {
		return ErrNoAWS
	}
	if err := s.Prov.Terminate(ctx, instanceID); err != nil {
		return err
	}
	s.Log.Info("terminated runner", "runner", name, "instance", instanceID, "reason", reason)
	s.note(ctx, "runner", "info", name, "Runner terminated", reason+" · "+instanceID)
	defer s.Bus.Publish("runner", name)
	return s.DB.Q.MarkTerminated(ctx, dbgen.MarkTerminatedParams{TerminatedAt: nullStr(ts(s.now())), ID: id})
}

// --- enrolment ---------------------------------------------------------------------

// Enroll validates a runner's enrolment and issues its NATS credentials. remoteIP is the
// TCP source address of the request.
func (s *Service) Enroll(ctx context.Context, req proto.EnrollRequest, remoteIP string) (*proto.EnrollResponse, error) {
	invalid := func(f string, a ...any) error { return fmt.Errorf("%w: "+f, append([]any{ErrForbidden}, a...)...) }
	if !nkeys.IsValidPublicUserKey(req.NKeyPublic) {
		return nil, fmt.Errorf("%w: nkey_public is not a NATS user key", ErrInvalid)
	}
	h := sha256.Sum256([]byte(req.Token))
	tok, err := s.DB.RQ.GetEnrollmentToken(ctx, h[:])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("unknown token")
	} else if err != nil {
		return nil, err
	}
	now := s.now()
	if tok.UsedAt.Valid {
		return nil, invalid("token already used")
	}
	if ts(now) > tok.ExpiresAt {
		return nil, invalid("token expired")
	}
	t, err := s.DB.RQ.GetTemplate(ctx, tok.TemplateID)
	if err != nil {
		return nil, err
	}
	if want := unameArch(t.Arch); req.Facts.Arch != want {
		return nil, invalid("arch %s does not match template %s (%s)", req.Facts.Arch, t.Name, want)
	}
	if int64(req.Facts.VCPU) < t.MinVcpu ||
		float64(req.Facts.RAMMiB) < float64(t.MinRamMib)*measureTolerance ||
		float64(req.Facts.DiskGiB) < float64(t.MinDiskGib)*measureTolerance {
		return nil, invalid("machine (%d vCPU, %d MiB, %d GiB) is below template %s minimums (%d vCPU, %d MiB, %d GiB)",
			req.Facts.VCPU, req.Facts.RAMMiB, req.Facts.DiskGiB, t.Name, t.MinVcpu, t.MinRamMib, t.MinDiskGib)
	}

	var existing *dbgen.Runner
	if tok.RunnerID.Valid { // controller-launched: bound to an instance
		r, err := s.DB.RQ.GetRunner(ctx, tok.RunnerID.Int64)
		if err != nil {
			return nil, err
		}
		if !tok.InstanceID.Valid || req.InstanceID != tok.InstanceID.String {
			return nil, invalid("token is bound to instance %s, request claims %q", tok.InstanceID.String, req.InstanceID)
		}
		// Inside the VPC there is no NAT: the request must come from that instance's IP.
		if r.PrivateIp == "" || remoteIP != r.PrivateIp {
			return nil, invalid("request from %s, but instance %s has private IP %q", remoteIP, req.InstanceID, r.PrivateIp)
		}
		existing = &r
	}

	var resp proto.EnrollResponse
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		var r dbgen.Runner
		if existing != nil {
			r = *existing
		} else {
			name := tok.RunnerName
			if name == "" {
				name = s.newName(ctx)
			}
			var err error
			if r, err = q.CreateAdoptedRunner(ctx, dbgen.CreateAdoptedRunnerParams{Name: name, TemplateID: t.ID}); err != nil {
				return err
			}
		}
		n, err := q.ConsumeEnrollmentToken(ctx, dbgen.ConsumeEnrollmentTokenParams{
			UsedAt: nullStr(ts(now)), RunnerID: sql.NullInt64{Int64: r.ID, Valid: true}, ID: tok.ID,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return invalid("token already used") // lost a race with a concurrent enrol
		}
		if err := q.EnrollRunner(ctx, dbgen.EnrollRunnerParams{
			NkeyPublic: nullStr(req.NKeyPublic), InstanceID: nullStr(req.InstanceID), Arch: t.Arch,
			Vcpu: int64(req.Facts.VCPU), RamMib: int64(req.Facts.RAMMiB), DiskGib: int64(req.Facts.DiskGiB),
			PrivateIp: remoteIP, EnrolledAt: nullStr(ts(now)), ID: r.ID,
		}); err != nil {
			return err
		}
		jwt, err := s.Keys.IssueRunner(r.ID, req.NKeyPublic)
		if err != nil {
			return err
		}
		resp = proto.EnrollResponse{RunnerID: r.ID, Name: r.Name, UserJWT: jwt, NATSURLs: s.NATSURLs, NATSCA: string(s.NATSCA)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Log.Info("runner enrolled", "runner", resp.Name, "id", resp.RunnerID, "from", remoteIP)
	s.note(ctx, "runner", "info", resp.Name, "Runner enrolled", "from "+remoteIP)
	return &resp, nil
}

// --- heartbeats, offline, reaper ------------------------------------------------------

// RunnerIDFromSubject parses wr.r.<id>.hb. The id comes from the subject, which NATS
// permissions bind to the sender, never from the payload.
func RunnerIDFromSubject(subject string) (int64, bool) {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != "wr" || parts[1] != "r" || parts[3] != "hb" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	return id, err == nil && id > 0
}

// Heartbeat records one heartbeat message.
func (s *Service) Heartbeat(ctx context.Context, subject string, data []byte) error {
	id, ok := RunnerIDFromSubject(subject)
	if !ok {
		return fmt.Errorf("%w: heartbeat subject %q", ErrInvalid, subject)
	}
	var hb proto.Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return fmt.Errorf("%w: heartbeat: %v", ErrInvalid, err)
	}
	metrics, _ := json.Marshal(hb)
	_, err := s.DB.Q.RecordHeartbeat(ctx, dbgen.RecordHeartbeatParams{Now: nullStr(ts(s.now())), Metrics: string(metrics), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return nil // terminated or unknown runner: ignore
	}
	if err != nil {
		return err
	}
	s.hist.add(id, hb, s.now())
	s.Bus.Publish("runner", strconv.FormatInt(id, 10))
	if err := s.reconcileBinary(ctx, id, hb); err != nil {
		s.Log.Warn("runner upgrade", "runner", id, "err", err)
	}
	return s.reconcileTemplate(ctx, id, hb)
}

// SweepOffline marks runners offline after proto.OfflineAfter without a heartbeat.
func (s *Service) SweepOffline(ctx context.Context) error {
	now := s.now()
	rows, err := s.DB.Q.MarkOffline(ctx, dbgen.MarkOfflineParams{
		Now: nullStr(ts(now)), Cutoff: nullStr(ts(now.Add(-proto.OfflineAfter))),
	})
	for _, r := range rows {
		s.Log.Warn("runner offline", "runner", r.Name)
		s.note(ctx, "runner", "warn", r.Name, "Runner offline", "no heartbeat")
		s.Bus.Publish("runner", r.Name)
		if s.OnOffline != nil {
			s.OnOffline(r.Name)
		}
	}
	return err
}

// Reap terminates controller-launched runners that have been idle (or offline) longer
// than their template's idle timeout, or never enrolled. It returns the names reaped.
func (s *Service) Reap(ctx context.Context) ([]string, error) {
	now := s.now()
	cands, err := s.DB.RQ.ReapCandidates(ctx, dbgen.ReapCandidatesParams{
		Now: ts(now), EnrollDeadline: nullStr(ts(now.Add(-EnrollDeadline))),
	})
	if err != nil {
		return nil, err
	}
	var reaped []string
	var errs []error
	for _, c := range cands {
		if c.State == "idle" && s.Attached != nil && s.Attached(c.ID) {
			continue // being watched or used through the web terminal
		}
		reason := map[string]string{
			"idle":    fmt.Sprintf("idle for over %s", time.Duration(c.IdleTimeoutS)*time.Second),
			"offline": fmt.Sprintf("offline for over %s", time.Duration(c.IdleTimeoutS)*time.Second),
		}[c.State]
		if reason == "" {
			reason = fmt.Sprintf("did not enrol within %s", EnrollDeadline)
		}
		if err := s.terminate(ctx, c.ID, c.Name, c.InstanceID.String, reason); err != nil {
			errs = append(errs, fmt.Errorf("reap %s: %w", c.Name, err))
			continue
		}
		reaped = append(reaped, c.Name)
	}
	return reaped, errors.Join(errs...)
}

// Loop runs the offline sweep and reaper every interval until ctx ends.
func (s *Service) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.SweepOffline(ctx); err != nil {
			s.Log.Error("offline sweep", "err", err)
		}
		if err := s.ReconcileTargets(ctx); err != nil {
			s.Log.Error("remote targets", "err", err)
		}
		if _, err := s.Reap(ctx); err != nil {
			s.Log.Error("reaper", "err", err)
		}
		if _, err := s.SweepDrained(ctx); err != nil {
			s.Log.Error("drain sweep", "err", err)
		}
	}
}
