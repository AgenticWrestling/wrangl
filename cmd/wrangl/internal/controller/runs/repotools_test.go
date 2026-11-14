package runs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func TestRepoToolsOnATwoRepoProject(t *testing.T) {
	m := newMaint(t) // project p links o/a
	ctx := context.Background()
	m.exec(`INSERT INTO repo (project_id, owner, name, note) VALUES (1, 'o', 'b', 'web client')`)
	m.exec(`UPDATE repo SET note = 'API server' WHERE name = 'a'`)
	m.exec(`UPDATE project SET default_repo_id = 2 WHERE id = 1`)
	if _, err := m.st.Create(ctx, stack.CreateParams{Project: "p", Parent: m.sqid, Title: "first", Actor: stack.User}); err != nil {
		t.Fatal(err)
	}
	run := m.startRun(time.Minute)
	sc, err := m.rs.ScopeForRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	call := func(tool, args string) (map[string]any, error) {
		out, err := m.rs.ExecTool(ctx, sc, tool, json.RawMessage(args))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(out)
		var mm map[string]any
		json.Unmarshal(b, &mm)
		return mm, nil
	}

	out, err := m.rs.ExecTool(ctx, sc, "repo_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if got := string(b); got != `[{"default":false,"default_branch":"main","note":"API server","repo":"o/a"},{"default":true,"default_branch":"main","note":"web client","repo":"o/b"}]` {
		t.Fatalf("repo_list: %s", got)
	}

	// A subtask with no repo inherits the project default; the explicit repo stays empty.
	kid, err := call("task_create", `{"parent":"`+m.sqid+`","title":"kid"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := call("task_get", `{"sqid":"`+kid["sqid"].(string)+`"}`)
	if got["repo"] != "" || got["effective_repo"] != "o/b" {
		t.Fatalf("task_get: %v", got)
	}
	// Naming a repo on create and on update.
	k2, err := call("task_create", `{"parent":"`+m.sqid+`","title":"api bit","repo":"o/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = call("task_get", `{"sqid":"`+k2["sqid"].(string)+`"}`)
	if got["repo"] != "o/a" || got["effective_repo"] != "o/a" {
		t.Fatalf("task_get after create: %v", got)
	}
	if _, err := call("task_update", `{"sqid":"`+k2["sqid"].(string)+`","repo":"o/b"}`); err != nil {
		t.Fatal(err)
	}
	got, _ = call("task_get", `{"sqid":"`+k2["sqid"].(string)+`"}`)
	if got["repo"] != "o/b" {
		t.Fatalf("task_get after update: %v", got)
	}
	if _, err := call("task_update", `{"sqid":"`+k2["sqid"].(string)+`","repo":""}`); err != nil {
		t.Fatal(err)
	}
	if got, _ = call("task_get", `{"sqid":"`+k2["sqid"].(string)+`"}`); got["repo"] != "" {
		t.Fatalf("repo not cleared: %v", got)
	}

	// An unknown repo is an error that lists the linked ones.
	_, err = call("task_create", `{"parent":"`+m.sqid+`","title":"x","repo":"o/nope"}`)
	if err == nil || !strings.Contains(err.Error(), "o/a, o/b") {
		t.Fatalf("unknown repo: %v", err)
	}
	// Outside the run's own subtree it may be read but not changed.
	other, err := m.st.Create(ctx, stack.CreateParams{Project: "p", Title: "elsewhere", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call("task_update", `{"sqid":"`+other.Sqid.String+`","repo":"o/a"}`); err == nil {
		t.Fatal("repo changed outside the subtree")
	}
}
