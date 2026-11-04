package gh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestApplyLabels(t *testing.T) {
	got := ApplyLabels([]string{"bug", "P1"}, []string{"help", "BUG"}, []string{"p1"})
	if !reflect.DeepEqual(got, []string{"bug", "help"}) {
		t.Fatalf("%v", got)
	}
}

func TestIssueWritesAgainstFakeGitHub(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var tokenPerms []map[string]string
	var calls []string
	denied := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.URL.Path == "/app/installations/7/access_tokens":
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.Unmarshal(b, &body)
			tokenPerms = append(tokenPerms, body.Permissions)
			if denied {
				http.Error(w, `{"message":"The permissions requested are not granted to this installation."}`, 422)
				return
			}
			if len(body.Repositories) != 1 || body.Repositories[0] != "app" {
				http.Error(w, "not one repo", 422)
				return
			}
			fmt.Fprint(w, `{"token":"ghs_issue","expires_at":"2030-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app/issues/9": // a pull request
			if r.Method == http.MethodGet {
				fmt.Fprint(w, `{"number":9,"title":"pr","state":"open","pull_request":{"url":"x"}}`)
				return
			}
			http.Error(w, "must not write to a PR", 500)
		case r.URL.Path == "/repos/acme/app/issues/1" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"number":1,"title":"t","state":"open","labels":[{"name":"bug"},{"name":"p1"}],"user":{"login":"ann"},"updated_at":"2026-11-04T10:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app/issues/1" && r.Method == http.MethodPatch:
			fmt.Fprint(w, `{"number":1,"title":"new","state":"closed","html_url":"u1","labels":[{"name":"bug"},{"name":"help"}],"updated_at":"2026-11-04T11:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app/issues" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"number":4,"title":"made","state":"open","html_url":"u4","updated_at":"2026-11-04T11:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app/issues/1/comments" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":55,"body":"hi","html_url":"u1#c55","user":{"login":"wrangl[bot]"}}`)
		case r.URL.Path == "/repos/acme/app/issues/2/comments" && r.Method == http.MethodPost:
			http.Error(w, `{"message":"Resource not accessible by integration"}`, 403)
		case r.URL.Path == "/repos/acme/app/issues/2":
			fmt.Fprint(w, `{"number":2,"title":"t","state":"open"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
		}
	}))
	defer srv.Close()
	c := &Client{AppID: 42, Key: key, BaseURL: srv.URL}
	ctx := context.Background()
	repo := Repo{Owner: "acme", Name: "app", InstallationID: 7}

	i, err := c.CreateIssue(ctx, repo, "made", "body", []string{"bug"})
	if err != nil || i.Number != 4 || i.UpdatedAt == "" {
		t.Fatalf("create: %+v %v", i, err)
	}
	if !strings.Contains(calls[len(calls)-1], `"labels":["bug"]`) || !strings.Contains(calls[len(calls)-1], `"title":"made"`) {
		t.Fatalf("create body: %s", calls[len(calls)-1])
	}

	title := "new"
	i, err = c.EditIssue(ctx, repo, 1, IssueEdit{Title: &title, AddLabels: []string{"help"}, RemoveLabels: []string{"p1"}})
	if err != nil || i.Title != "new" {
		t.Fatalf("edit: %+v %v", i, err)
	}
	if last := calls[len(calls)-1]; !strings.HasPrefix(last, "PATCH ") || !strings.Contains(last, `"labels":["bug","help"]`) || strings.Contains(last, `"body"`) {
		t.Fatalf("edit body: %s", last)
	}

	if i, err = c.CloseIssue(ctx, repo, 1, "not_planned"); err != nil || i.State != "closed" {
		t.Fatalf("close: %+v %v", i, err)
	}
	if last := calls[len(calls)-1]; !strings.Contains(last, `"state":"closed"`) || !strings.Contains(last, `"state_reason":"not_planned"`) {
		t.Fatalf("close body: %s", last)
	}

	cm, err := c.CreateComment(ctx, repo, 1, "hi")
	if err != nil || cm.ID != 55 || cm.URL != "u1#c55" {
		t.Fatalf("comment: %+v %v", cm, err)
	}

	// Writes use a token limited to issues: write; the only other tokens are the read guard's.
	sawWrite := false
	for _, p := range tokenPerms {
		switch {
		case reflect.DeepEqual(p, map[string]string{"issues": "write"}):
			sawWrite = true
		case reflect.DeepEqual(p, map[string]string{"contents": "write", "pull_requests": "write"}):
		default:
			t.Fatalf("unexpected token permissions %v", p)
		}
	}
	if !sawWrite {
		t.Fatal("no issues: write token was requested")
	}

	// Pull requests are never edited, closed or commented on through the issue endpoints.
	for name, f := range map[string]func() error{
		"edit":    func() error { _, err := c.EditIssue(ctx, repo, 9, IssueEdit{Title: &title}); return err },
		"close":   func() error { _, err := c.CloseIssue(ctx, repo, 9, ""); return err },
		"comment": func() error { _, err := c.CreateComment(ctx, repo, 9, "x"); return err },
	} {
		if err := f(); !errors.Is(err, ErrIsPullRequest) {
			t.Errorf("%s on a PR: %v", name, err)
		}
	}

	// 403 on the write is the permission error; so is a refused token request.
	if _, err := c.CreateComment(ctx, repo, 2, "x"); !errors.Is(err, ErrIssuePermission) {
		t.Fatalf("403: %v", err)
	}
	denied = true
	if _, err := c.CreateIssue(ctx, repo, "x", "", nil); !errors.Is(err, ErrIssuePermission) {
		t.Fatalf("token refused: %v", err)
	}
}
