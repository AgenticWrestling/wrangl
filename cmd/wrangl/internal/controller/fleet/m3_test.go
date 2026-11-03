package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
)

func TestUpdateTemplateBumpsVersionAndClearsFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tmpl, _ := e.s.Template(ctx, "go-claude")
	e.s.DB.W.Exec(`UPDATE runner_template SET failed_version = version WHERE id = ?`, tmpl.ID)
	up, err := e.s.UpdateTemplate(ctx, "go-claude", []byte(strings.Replace(tmplYAML, `go = "1.26"`, `go = "1.27"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if up.Version != tmpl.Version+1 || up.FailedVersion != 0 || !strings.Contains(up.MiseToml, "1.27") {
		t.Fatalf("%+v", up)
	}
	if _, err := e.s.UpdateTemplate(ctx, "go-claude", []byte(strings.Replace(tmplYAML, "name: go-claude", "name: other", 1))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename via edit: %v", err)
	}
	if _, err := e.s.UpdateTemplate(ctx, "nope", []byte(strings.Replace(tmplYAML, "name: go-claude", "name: nope", 1))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing template: %v", err)
	}
}

func TestTemplateResultSuccessAndFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, _, resp := e.launchAndEnroll(t)
	id := resp.RunnerID
	e.s.DB.W.Exec(`UPDATE runner SET state = 'provisioning' WHERE id = ?`, id)
	subj := natsd.RunnerTemplateEventSubject(id)

	if err := e.s.TemplateResult(ctx, subj, []byte(`{"version":1,"ok":true,"log":"done"}`)); err != nil {
		t.Fatal(err)
	}
	r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name)
	if r.State != "idle" || r.TemplateVersion != 1 || !r.IdleSince.Valid || r.Cordoned != 0 {
		t.Fatalf("after success: %+v", r)
	}

	e.s.DB.W.Exec(`UPDATE runner SET state = 'provisioning' WHERE id = ?`, id)
	if err := e.s.TemplateResult(ctx, subj, []byte(`{"version":1,"ok":false,"error":"mise install: exit 1","log":"boom"}`)); err != nil {
		t.Fatal(err)
	}
	r, _ = e.s.DB.RQ.GetRunnerByName(ctx, name)
	tmpl, _ := e.s.Template(ctx, "go-claude")
	if r.Cordoned != 1 || r.State != "idle" || r.LastError == "" || r.TemplateLog != "boom" || tmpl.FailedVersion != 1 {
		t.Fatalf("after failure: runner %+v template failed_version %d", r, tmpl.FailedVersion)
	}
	// A failed, cordoned runner is still reaped after the idle timeout.
	e.clk.Advance(31 * time.Minute)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 1 {
		t.Fatalf("failed runner not reaped: %v", reaped)
	}
	if err := e.s.TemplateResult(ctx, "wr.evt.x.template", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad subject: %v", err)
	}
}

func TestCordonUncordonDrain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, inst, resp := e.launchAndEnroll(t)
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))

	if err := e.s.Cordon(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	if done, _ := e.s.SweepDrained(ctx); len(done) != 0 {
		t.Fatal("cordon alone must not terminate")
	}
	if err := e.s.Cordon(ctx, name, false); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name); r.Cordoned != 0 {
		t.Fatal("uncordon failed")
	}
	if err := e.s.Drain(ctx, name); err != nil {
		t.Fatal(err)
	}
	done, err := e.s.SweepDrained(ctx)
	if err != nil || len(done) != 1 || e.ec2.State(inst) != "terminated" {
		t.Fatalf("drain sweep: %v %v", done, err)
	}
	if err := e.s.Cordon(ctx, "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown runner: %v", err)
	}
}
