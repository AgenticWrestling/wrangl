package runs_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
)

func TestNotificationHookIsStoredAndRaisesANotice(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO runner (name, template_id) VALUES ('r1', 1)`,
		`INSERT INTO project (slug, name) VALUES ('p','p')`,
		`INSERT INTO task (project_id, title, rank) VALUES (1, 't', 'A')`,
		`INSERT INTO run (task_id, runner_id, state) VALUES (1, 1, 'running')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var gotRun int64
	var gotMsg string
	s := &runs.Service{DB: d, Bus: &events.Bus{}, OnAgentNotice: func(r dbgen.Run, m string) { gotRun, gotMsg = r.ID, m }}

	seq := 0
	send := func(event, payload string) {
		seq++
		b, _ := json.Marshal(map[string]any{"seq": seq, "ts": time.Now().UTC(), "event": event, "payload": json.RawMessage(payload)})
		if err := s.HandleHook(ctx, "wr.evt.1.1.hook", b); err != nil {
			t.Fatal(err)
		}
	}
	send("PostToolUse", `{"tool_name":"Bash"}`)
	if gotRun != 0 {
		t.Fatal("only Notification raises a notice")
	}
	send("Notification", `{"message":"Claude needs your permission to use Bash"}`)
	if gotRun != 1 || gotMsg != "Claude needs your permission to use Bash" {
		t.Fatalf("notice: run %d %q", gotRun, gotMsg)
	}
	evs, err := d.RQ.RunHookEvents(ctx, 1)
	if err != nil || len(evs) != 2 || evs[1].Name != "Notification" {
		t.Fatalf("stored events: %+v %v", evs, err)
	}
}
