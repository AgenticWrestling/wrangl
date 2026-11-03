package prs

import (
	"strings"
	"testing"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/gh"
)

func TestProtectedGlobs(t *testing.T) {
	globs := []string{".github/**", "**/migrations/**", "go.mod", "**/*.tf", "Dockerfile*"}
	files := []gh.PRFile{
		{Path: ".github/workflows/ci.yml"}, {Path: "db/migrations/0001.sql"}, {Path: "migrations/x.sql"},
		{Path: "go.mod"}, {Path: "infra/main.tf"}, {Path: "main.tf"}, {Path: "Dockerfile.dev"},
		{Path: "cmd/go.mod.bak"}, {Path: "src/main.go"}, {Path: "docs/github.md"},
	}
	got := strings.Join(Protected(files, globs), ",")
	want := ".github/workflows/ci.yml,db/migrations/0001.sql,migrations/x.sql,go.mod,infra/main.tf,main.tf,Dockerfile.dev"
	if got != want {
		t.Fatalf("protected:\n got %s\nwant %s", got, want)
	}
}

func TestCanReview(t *testing.T) {
	s := &Service{}
	if s.canReview("o") {
		t.Fatal("no reviewer client")
	}
	s.Reviewer = &gh.Fake{}
	if !s.canReview("o") {
		t.Fatal("a plain reviewer client serves every owner")
	}
	s.HasReviewer = func(o string) bool { return o == "mine" }
	if s.canReview("other") || !s.canReview("mine") {
		t.Fatal("per-owner reviewer Apps")
	}
	if s.gh(gh.Repo{Owner: "other"}) != s.Author {
		t.Fatal("owners without a reviewer App are read through the author App")
	}
}
