package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func TestTargetRoutesByRunID(t *testing.T) {
	w := &Worker{}
	if _, err := w.target(0); err != errNoRun {
		t.Fatalf("no runs: %v", err)
	}
	w.runs = map[int64]*runState{7: {id: 7, kind: "work"}}
	if rs, err := w.target(0); err != nil || rs.id != 7 {
		t.Fatalf("a sole run is the default: %v %v", rs, err)
	}
	w.runs[9] = &runState{id: 9, kind: "review"}
	if _, err := w.target(0); err == nil || !strings.Contains(err.Error(), "which") {
		t.Fatalf("several runs and no id must be refused, not guessed: %v", err)
	}
	if rs, err := w.target(9); err != nil || rs.kind != "review" {
		t.Fatalf("named run: %v %v", rs, err)
	}
	if _, err := w.target(8); err == nil {
		t.Fatal("a run that is not here must be refused")
	}
	if got := w.Active(); len(got) != 2 || got[0] != 7 || got[1] != 9 {
		t.Fatalf("active %v", got)
	}
}

func TestStartRespectsSessionLimit(t *testing.T) {
	w := &Worker{runs: map[int64]*runState{1: {id: 1}, 2: {id: 2}}}
	ctx := context.Background()
	if err := w.Start(ctx, proto.Assignment{RunID: 3, MaxSessions: 2}); err == nil || !strings.Contains(err.Error(), "2 of 2") {
		t.Fatalf("a full runner must decline: %v", err)
	}
	if err := w.Start(ctx, proto.Assignment{RunID: 3}); err == nil { // no limit sent = one session
		t.Fatal("an assignment without MaxSessions means one session")
	}
	if err := w.Start(ctx, proto.Assignment{RunID: 1, MaxSessions: 9}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("the same run twice: %v", err)
	}
}

func TestQuestionsAreCancelledPerRun(t *testing.T) {
	var a waiters
	c1, c2 := a.add(10, 1), a.add(20, 2)
	a.cancelRun(1)
	select {
	case ans := <-c1:
		if !ans.Cancelled || ans.QuestionID != 10 {
			t.Fatalf("%+v", ans)
		}
	default:
		t.Fatal("run 1's question was not cancelled")
	}
	select {
	case ans := <-c2:
		t.Fatalf("run 2's question must survive run 1 ending: %+v", ans)
	default:
	}
	if !a.deliver(proto.Answer{QuestionID: 20, Value: "x"}) {
		t.Fatal("run 2's question should still be answerable")
	}
}

// A secret file written to a shared path stays until the last session using it ends.
func TestSharedSecretFileLivesUntilLastSession(t *testing.T) {
	w := &Worker{}
	p := filepath.Join(t.TempDir(), "cred.json")
	if w.holdSecret(p) {
		t.Fatal("first holder must write the file")
	}
	os.WriteFile(p, []byte("s3cret"), 0o600)
	if !w.holdSecret(p) {
		t.Fatal("second holder must reuse it")
	}
	w.dropSecret(p)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("removed while a session still uses it: %v", err)
	}
	w.dropSecret(p)
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the last session out must remove it")
	}
}
