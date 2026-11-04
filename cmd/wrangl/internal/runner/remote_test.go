package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func remoteWorker(t *testing.T, fake *FakeTarget) (*Worker, *runState, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package x"), 0o644)
	w := &Worker{Cfg: &Config{StateDir: t.TempDir()}, Targets: fake}
	s := &session{w: w, dir: dir, a: proto.Assignment{RunID: 5, Target: &proto.TargetConn{Name: "mac1", Protocol: "ssh", User: "ec2-user", Port: 22, Key: "PRIVATE-KEY", Reset: "make clean"}}}
	rt, err := w.openTarget(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.close)
	s.target = rt
	rs := &runState{id: 5, sess: s}
	w.runs = map[int64]*runState{5: rs}
	return w, rs, dir
}

func call(w *Worker, name string, args any) (map[string]any, error) {
	b, _ := json.Marshal(args)
	out, err := w.ToolFor(context.Background(), 5, name, b)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	return m, nil
}

func TestTargetExecAndPut(t *testing.T) {
	fake := &FakeTarget{Handler: func(cmd string) (string, int) {
		if strings.HasPrefix(cmd, "xcodebuild") {
			return "BUILD FAILED\n", 65
		}
		return "ok\n", 0
	}}
	w, _, _ := remoteWorker(t, fake)
	res, err := call(w, "target_exec", map[string]any{"cmd": "xcodebuild test"})
	if err != nil || res["exit_code"].(float64) != 65 || !strings.Contains(res["output"].(string), "BUILD FAILED") {
		t.Fatalf("exec: %v %v", res, err)
	}
	if fake.LastKey != "PRIVATE-KEY\n" {
		t.Fatalf("the daemon must give the client the key file, got %q", fake.LastKey)
	}
	if _, err := call(w, "target_put", map[string]any{"path": "main.go", "dest": "/tmp/main.go"}); err != nil || len(fake.Puts) != 1 || fake.Puts[0][1] != "/tmp/main.go" {
		t.Fatalf("put: %v %v", err, fake.Puts)
	}
	// target_put cannot reach outside the checkout
	if _, err := call(w, "target_put", map[string]any{"path": "../../etc/passwd", "dest": "/tmp/x"}); err == nil {
		t.Fatal("put outside the checkout must be refused")
	}
	if fake.Opened != 1 {
		t.Fatalf("tunnel opened %d times", fake.Opened)
	}
}

func TestTargetLostIsReportedAndKeyRemoved(t *testing.T) {
	fake := &FakeTarget{}
	w, rs, _ := remoteWorker(t, fake)
	keyFile := rs.sess.target.ep.KeyFile
	fake.Down = true
	if _, err := call(w, "target_exec", map[string]any{"cmd": "true"}); err == nil || !strings.Contains(err.Error(), "target lost") {
		t.Fatalf("want target lost, got %v", err)
	}
	rs.sess.target.reset(context.Background())
	rs.sess.target.close()
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key file must be removed when the run ends (%v)", err)
	}
	if fake.Closed != 1 {
		t.Fatalf("tunnel closed %d times", fake.Closed)
	}
}

func TestTargetToolsNeedATarget(t *testing.T) {
	w := &Worker{Cfg: &Config{}}
	w.runs = map[int64]*runState{5: {id: 5, sess: &session{}}}
	if _, err := call(w, "target_exec", map[string]any{"cmd": "ls"}); err == nil || !strings.Contains(err.Error(), "no remote target") {
		t.Fatalf("got %v", err)
	}
}
