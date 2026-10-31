package gh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestClientPRFlow(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var mu sync.Mutex
	var calls []string
	checkRuns := `{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}`
	statuses := `{"state":"success","statuses":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/repos/acme/app/installation":
			fmt.Fprint(w, `{"id": 909}`) // the reviewer App's own installation
		case r.URL.Path == "/app/installations/909/access_tokens":
			fmt.Fprint(w, `{"token":"ghs_rev","expires_at":"2030-01-01T00:00:00Z"}`)
		case r.Header.Get("Authorization") != "token ghs_rev":
			http.Error(w, "bad token", 401)
		case r.Method == "POST" && r.URL.Path == "/repos/acme/app/pulls":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["head"] != "wrangl/x-1" || b["base"] != "main" {
				http.Error(w, "bad pr", 422)
				return
			}
			fmt.Fprint(w, `{"number":5,"html_url":"https://github.com/acme/app/pull/5","state":"open","head":{"sha":"abc"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/acme/app/pulls/5":
			if r.Header.Get("Accept") == "application/vnd.github.diff" {
				fmt.Fprint(w, "diff --git a/x b/x\n")
				return
			}
			fmt.Fprint(w, `{"number":5,"state":"closed","merged":true,"head":{"sha":"abc"}}`)
		case r.URL.Path == "/repos/acme/app/pulls/5/files":
			fmt.Fprint(w, `[{"filename":"main.go","additions":10,"deletions":2}]`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, checkRuns)
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, statuses)
		case r.Method == "POST" && r.URL.Path == "/repos/acme/app/pulls/5/reviews",
			r.Method == "PUT" && r.URL.Path == "/repos/acme/app/pulls/5/merge",
			r.Method == "PATCH" && r.URL.Path == "/repos/acme/app/pulls/5":
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
		}
	}))
	defer srv.Close()
	instCache = sync.Map{}
	c := &Client{AppID: 77, Key: key, BaseURL: srv.URL}
	ctx := context.Background()
	repo := Repo{Owner: "acme", Name: "app", InstallationID: 1} // the author's id must not be used

	pr, err := c.CreatePR(ctx, repo, "wrangl/x-1", "main", "t", "b")
	if err != nil || pr.Number != 5 || pr.HeadSHA != "abc" {
		t.Fatalf("create: %+v %v", pr, err)
	}
	if got, _ := c.GetPR(ctx, repo, 5); !got.Merged {
		t.Fatalf("get: %+v", got)
	}
	if files, _ := c.PRFiles(ctx, repo, 5); len(files) != 1 || files[0].Additions != 10 {
		t.Fatalf("files: %+v", files)
	}
	if d, _ := c.PRDiff(ctx, repo, 5); !strings.HasPrefix(d, "diff --git") {
		t.Fatalf("diff: %q", d)
	}
	for _, tc := range []struct{ runs, status, want string }{
		{`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}`, `{"state":"success","statuses":[]}`, ChecksSuccess},
		{`{"total_count":1,"check_runs":[{"status":"in_progress"}]}`, `{"state":"success","statuses":[]}`, ChecksPending},
		{`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"failure"}]}`, `{"state":"success","statuses":[]}`, ChecksFailure},
		{`{"total_count":0,"check_runs":[]}`, `{"state":"pending","statuses":[{}]}`, ChecksPending},
		{`{"total_count":0,"check_runs":[]}`, `{"state":"pending","statuses":[]}`, ChecksNone},
	} {
		checkRuns, statuses = tc.runs, tc.status
		if got, err := c.Checks(ctx, repo, "abc"); err != nil || got != tc.want {
			t.Errorf("checks %s / %s: %s %v, want %s", tc.runs, tc.status, got, err, tc.want)
		}
	}
	for _, f := range []func() error{
		func() error { return c.Review(ctx, repo, 5, "APPROVE", "ok") },
		func() error { return c.Merge(ctx, repo, 5, "t") },
		func() error { return c.ClosePR(ctx, repo, 5) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(calls, "\n"), "PUT /repos/acme/app/pulls/5/merge") {
		t.Fatalf("calls: %v", calls)
	}
}
