package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

type fakeTasks struct {
	tasks   map[string]proto.TaskDetail
	order   []string
	created []proto.TaskCreateRequest
	queued  []string
	queueEr error
	n       int
}

func newFakeTasks() *fakeTasks { return &fakeTasks{tasks: map[string]proto.TaskDetail{}} }

func (f *fakeTasks) Tasks(_ context.Context, project, state, parent string) ([]proto.TaskRef, error) {
	var out []proto.TaskRef
	for _, s := range f.order {
		if t := f.tasks[s]; t.Project == project {
			out = append(out, proto.TaskRef{Sqid: s, Title: t.Title, State: t.State})
		}
	}
	return out, nil
}

func (f *fakeTasks) Task(_ context.Context, sqid string) (proto.TaskDetail, error) {
	t, ok := f.tasks[sqid]
	if !ok {
		return t, errors.New("not found")
	}
	return t, nil
}

func (f *fakeTasks) CreateTask(_ context.Context, req proto.TaskCreateRequest) (proto.TaskRef, error) {
	f.created = append(f.created, req)
	f.n++
	sq := "t" + string(rune('a'+f.n))
	f.tasks[sq] = proto.TaskDetail{Sqid: sq, Project: req.Project, Parent: req.Parent, Title: req.Title, Prompt: req.Prompt, Type: req.Type, State: "open"}
	f.order = append(f.order, sq)
	return proto.TaskRef{Sqid: sq, Title: req.Title, State: "open"}, nil
}

func (f *fakeTasks) QueueTask(_ context.Context, sqid string) (int64, string, error) {
	if f.queueEr != nil {
		return 0, "", f.queueEr
	}
	f.queued = append(f.queued, sqid)
	return 42, "wrangl/x", nil
}

func TestDelegateCreatesFeatureOnceAndQueues(t *testing.T) {
	f := newFakeTasks()
	ctx := context.Background()
	t1, run, err := Delegate(ctx, f, "proj", "fix the thing", "details", "", true)
	if err != nil || run != 42 {
		t.Fatalf("%v %d", err, run)
	}
	if len(f.created) != 2 || f.created[0].Title != DelegatedTitle || f.created[0].Type != "feature" || f.created[0].Parent != "" {
		t.Fatalf("feature not created first: %+v", f.created)
	}
	if c := f.created[1]; c.Parent != "tb" || c.Title != "fix the thing" || c.Prompt != "details" || c.Type != "code" {
		t.Errorf("task request: %+v", c)
	}
	if len(f.queued) != 1 || f.queued[0] != t1.Sqid {
		t.Errorf("queued %v", f.queued)
	}
	// The second delegate reuses the feature, does not queue without the flag, and honours --type.
	_, run, err = Delegate(ctx, f, "proj", "another", "", "docs", false)
	if err != nil || run != 0 || len(f.created) != 3 || f.created[2].Parent != "tb" || f.created[2].Type != "docs" || len(f.queued) != 1 {
		t.Fatalf("second: %v run %d %+v queued %v", err, run, f.created, f.queued)
	}
	// Another project gets its own feature; a cancelled Delegated feature is not reused.
	if _, _, err := Delegate(ctx, f, "other", "x", "", "", false); err != nil || f.created[3].Title != DelegatedTitle {
		t.Fatalf("other project: %v %+v", err, f.created)
	}
	d := f.tasks["tb"]
	d.State = "cancelled"
	f.tasks["tb"] = d
	Delegate(ctx, f, "proj", "y", "", "", false)
	if last := f.created[len(f.created)-2]; last.Title != DelegatedTitle {
		t.Errorf("cancelled feature reused: %+v", f.created)
	}
}

func TestDelegateQueueFailureKeepsTheTask(t *testing.T) {
	f := newFakeTasks()
	f.queueEr = errors.New("no template")
	task, _, err := Delegate(context.Background(), f, "p", "t", "", "", true)
	if err == nil || task.Sqid == "" || !contains(err.Error(), "created "+task.Sqid) {
		t.Fatalf("%v %+v", err, task)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestInferProject(t *testing.T) {
	f := newFakeTasks()
	f.tasks["tz"] = proto.TaskDetail{Sqid: "tz", Project: "from-task"}
	t.Setenv("WRANGL_PROJECT", "")
	t.Setenv("WRANGL_TASK", "")
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(t.TempDir())
	if _, err := inferProject(context.Background(), f); err == nil {
		t.Error("no hint should be an error")
	}
	prim := filepath.Join(t.TempDir(), "work", "primary", "demo", "sub")
	os.MkdirAll(prim, 0o755)
	os.Chdir(prim)
	if p, _ := inferProject(context.Background(), f); p != "demo" {
		t.Errorf("cwd: %q", p)
	}
	t.Setenv("WRANGL_TASK", "tz")
	if p, _ := inferProject(context.Background(), f); p != "from-task" {
		t.Errorf("task: %q", p)
	}
	t.Setenv("WRANGL_PROJECT", "explicit")
	if p, _ := inferProject(context.Background(), f); p != "explicit" {
		t.Errorf("env: %q", p)
	}
}

func TestClassify(t *testing.T) {
	run := func(id int64, st string) proto.TaskRunView { return proto.TaskRunView{ID: id, State: st} }
	for _, c := range []struct {
		t    proto.TaskDetail
		want string
		done bool
	}{
		{proto.TaskDetail{State: "done"}, "done", true},
		{proto.TaskDetail{State: "in_review"}, "review", true},
		{proto.TaskDetail{State: "cancelled"}, "cancelled", true},
		{proto.TaskDetail{State: "open"}, "", false},
		{proto.TaskDetail{State: "open", Runs: []proto.TaskRunView{run(1, "queued")}}, "", false},
		{proto.TaskDetail{State: "in_progress", Runs: []proto.TaskRunView{run(1, "running")}}, "", false},
		{proto.TaskDetail{State: "open", Runs: []proto.TaskRunView{run(1, "failed")}}, "failed", true},
		{proto.TaskDetail{State: "open", Runs: []proto.TaskRunView{run(2, "queued"), run(1, "failed")}}, "", false}, // retried
	} {
		if got, done := classify(c.t); got != c.want || done != c.done {
			t.Errorf("%+v: %q %v, want %q %v", c.t, got, done, c.want, c.done)
		}
	}
}

func TestWaitForTask(t *testing.T) {
	states := []string{"open", "in_progress", "in_progress", "in_review"}
	i := 0
	fetch := func(context.Context, string) (proto.TaskDetail, error) {
		s := states[min(i, len(states)-1)]
		i++
		return proto.TaskDetail{Sqid: "x", State: s, Summary: "all good"}, nil
	}
	r, err := WaitForTask(context.Background(), fetch, "x", time.Second, time.Millisecond)
	if err != nil || r.Outcome != "review" || r.Failed() || r.Task.Summary != "all good" || i != 4 {
		t.Fatalf("%+v %v polls=%d", r, err, i)
	}
	// Timeout: reports the last state, no error.
	start := time.Now()
	r, err = WaitForTask(context.Background(), func(context.Context, string) (proto.TaskDetail, error) {
		return proto.TaskDetail{Sqid: "x", State: "in_progress"}, nil
	}, "x", 60*time.Millisecond, 10*time.Millisecond)
	if err != nil || r.Outcome != "timeout" || !r.Failed() || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v %v", r, err)
	}
	// A fetch error is returned; a cancelled context ends the wait.
	if _, err := WaitForTask(context.Background(), func(context.Context, string) (proto.TaskDetail, error) { return proto.TaskDetail{}, errors.New("boom") }, "x", 0, time.Millisecond); err == nil {
		t.Error("fetch error swallowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := WaitForTask(ctx, func(context.Context, string) (proto.TaskDetail, error) {
		return proto.TaskDetail{State: "open"}, nil
	}, "x", 0, time.Hour); err == nil {
		t.Error("cancelled context not reported")
	}
}

func TestOutcomePrinting(t *testing.T) {
	var b stringsBuilder
	printOutcome(&b, WaitResult{Outcome: "review", Task: proto.TaskDetail{Sqid: "ab12", Title: "T", State: "in_review", Summary: "did it", Runs: []proto.TaskRunView{{ID: 5, State: "succeeded", Branch: "wrangl/t", Pushed: true}}}})
	for _, w := range []string{"ab12 T: finished, waiting for your review", "summary\tdid it", "run 5\tsucceeded · wrangl/t (pushed)"} {
		if !contains(b.s, w) {
			t.Errorf("missing %q in %q", w, b.s)
		}
	}
}

type stringsBuilder struct{ s string }

func (b *stringsBuilder) Write(p []byte) (int, error) { b.s += string(p); return len(p), nil }
