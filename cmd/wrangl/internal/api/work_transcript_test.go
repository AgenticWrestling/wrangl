package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/transcript"
)

// transcriptServer is archiveServer plus an embedded JetStream with WR_EVT, a runner
// and two runs: 1 (running, runner 7) and 2 (queued: not started).
func transcriptServer(t *testing.T) (*http.ServeMux, jetstream.JetStream) {
	t.Helper()
	mux, d := archiveServer(t)
	for _, q := range []string{
		`INSERT INTO runner_template (id, name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES (1, 't', '', '{}', 1, 1, 1)`,
		`INSERT INTO runner (id, name, template_id) VALUES (7, 'r7', 1)`,
		`INSERT INTO run (id, task_id, runner_id, state) VALUES (1, 1, 7, 'running')`,
		`INSERT INTO run (id, task_id, state) VALUES (2, 1, 'queued')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e, err := natsd.Start(t.TempDir(), "127.0.0.1:-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	js, _ := jetstream.New(e.Conn)
	if _, err := js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{Name: "WR_EVT", Subjects: []string{"wr.evt.>"}}); err != nil {
		t.Fatal(err)
	}
	mux = http.NewServeMux()
	registerWork(mux, Deps{Fleet: &fleet.Service{DB: d}, JS: js})
	return mux, js
}

func pubEntries(t *testing.T, js jetstream.JetStream, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		b, _ := json.Marshal(transcript.Entry{Seq: i, Role: "assistant", Kind: "text", Text: fmt.Sprintf("m%d", i)})
		if _, err := js.Publish(context.Background(), natsd.RunTranscriptSubject(7, 1), b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTranscriptJSONTailAndPaging(t *testing.T) {
	mux, js := transcriptServer(t)
	pubEntries(t, js, 1, 10)
	get := func(q string) TranscriptPage {
		rec := call(mux, "GET", "/api/v1/runs/1/transcript"+q)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		var p TranscriptPage
		json.Unmarshal(rec.Body.Bytes(), &p)
		return p
	}
	p := get("?limit=4")
	if len(p.Entries) != 4 || p.Entries[0].Seq != 7 || p.Entries[3].Seq != 10 || !p.HasMore {
		t.Fatalf("tail: %+v", p)
	}
	p = get("?limit=4&before=7")
	if len(p.Entries) != 4 || p.Entries[0].Seq != 3 || p.Entries[3].Seq != 6 || !p.HasMore {
		t.Fatalf("page 2: %+v", p)
	}
	p = get("?limit=4&before=3")
	if len(p.Entries) != 2 || p.Entries[0].Seq != 1 || p.HasMore {
		t.Fatalf("last page: %+v", p)
	}
	if p = get("?before=1"); len(p.Entries) != 0 || p.HasMore {
		t.Fatalf("before 1: %+v", p)
	}
}

func TestTranscriptUnknownAndNotStarted(t *testing.T) {
	mux, _ := transcriptServer(t)
	if rec := call(mux, "GET", "/api/v1/runs/99/transcript"); rec.Code != 404 {
		t.Errorf("unknown run: %d", rec.Code)
	}
	if rec := call(mux, "GET", "/api/v1/runs/abc/transcript"); rec.Code != 400 {
		t.Errorf("bad id: %d", rec.Code)
	}
	if rec := call(mux, "GET", "/api/v1/runs/2/transcript"); rec.Code != 409 {
		t.Errorf("not started: %d", rec.Code)
	}
}

// sseIDs reads SSE entry ids from a running request until n are seen.
func sseIDs(t *testing.T, srv *httptest.Server, path, lastID string, n int) []string {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	var ids []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && len(ids) < n {
		if id, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestTranscriptSSEResume(t *testing.T) {
	mux, js := transcriptServer(t)
	pubEntries(t, js, 1, 5)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	const p = "/api/v1/runs/1/transcript?format=sse"

	if got := strings.Join(sseIDs(t, srv, p, "", 5), ","); got != "1,2,3,4,5" {
		t.Fatalf("full replay: %s", got)
	}
	if got := strings.Join(sseIDs(t, srv, p, "3", 2), ","); got != "4,5" {
		t.Fatalf("resume after 3: %s", got)
	}
	if got := strings.Join(sseIDs(t, srv, p+"&limit=2", "", 2), ","); got != "4,5" {
		t.Fatalf("limit replay: %s", got)
	}
	// Follow live: entries published after the connection opens arrive.
	go func() { time.Sleep(300 * time.Millisecond); pubEntries(t, js, 6, 7) }()
	if got := strings.Join(sseIDs(t, srv, p, "5", 2), ","); got != "6,7" {
		t.Fatalf("live follow: %s", got)
	}
}
