package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// fakeAPI serves one open task_review question and records the answer posted to it.
func fakeAPI(t *testing.T, got *proto.AnswerRequest) string {
	t.Helper()
	q := proto.QuestionView{ID: 7, Kind: "task_review", State: "open", Task: "abc123", TaskTitle: "ship it", Project: "p",
		Prompt: `Review abc123 "ship it"`, DetailMD: "**Agent's summary**\n\nalready built", AllowFreeText: true,
		Options: []proto.QuestionOption{{Label: "Accept", Value: "accept"}, {Label: "Reopen", Value: "reopen"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/questions":
			json.NewEncoder(w).Encode([]proto.QuestionView{q})
		case r.URL.Path == "/api/v1/questions/7" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(q)
		case r.URL.Path == "/api/v1/questions/7/answer":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, got)
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestQuestionTaskReview(t *testing.T) {
	var got proto.AnswerRequest
	url := fakeAPI(t, &got)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out, err := run2(t, url, "question", "list")
	if err != nil || !strings.Contains(out, "task_review") {
		t.Fatalf("list: %v %q", err, out)
	}
	out, err = run2(t, url, "question", "show", "7")
	if err != nil || !strings.Contains(out, "accept: Accept") || !strings.Contains(out, "reopen: Reopen") || !strings.Contains(out, "already built") {
		t.Fatalf("show: %v %q", err, out)
	}
	if _, err = run2(t, url, "question", "answer", "7", "--option", "accept"); err != nil || got.Value != "accept" {
		t.Fatalf("accept: %v %+v", err, got)
	}
	got = proto.AnswerRequest{}
	if _, err = run2(t, url, "question", "answer", "7", "--option", "reopen", "--text", "missing docs"); err != nil || got.Value != "reopen" || got.Text != "missing docs" {
		t.Fatalf("reopen: %v %+v", err, got)
	}
	if _, err = run2(t, url, "question", "answer", "7", "accept", "--option", "reopen"); err == nil {
		t.Fatal("conflicting option and argument must be refused")
	}
}

func run2(t *testing.T, controller string, args ...string) (string, error) {
	t.Helper()
	return run(t, "", append([]string{"--controller", controller}, args...)...)
}
