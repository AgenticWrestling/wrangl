package web

// Actions: actions on projects and their repos.

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func actProjectNew(h *Handler, a *actReq) (res actResult, err error) {
	slug := a.str("slug")
	pp := projectsParams(a.str("ptemplate"))
	if n := a.str("pname"); n != "" {
		pp.Name = &n
	}
	if g := a.str("pgoals"); g != "" {
		pp.Description = &g
	}
	if _, err = h.D.Projects.Create(a.ctx, slug, pp); err != nil {
		return
	}
	res.msg, res.nav = "project created", "/projects/"+url.PathEscape(slug)+"/settings"
	var failed []string
	for _, ref := range strings.FieldsFunc(a.str("prepos"), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if _, lerr := h.D.Projects.LinkRepo(a.ctx, slug, ref, ""); lerr != nil {
			failed = append(failed, ref+": "+lerr.Error())
		}
	}
	if len(failed) > 0 {
		res.msg = "project created; not linked: " + strings.Join(failed, "; ")
	}
	res.reset = map[string]any{"slug": "", "pname": "", "pgoals": "", "prepos": "", "ptemplate": ""}
	return
}

func actProjectSave(h *Handler, a *actReq) (res actResult, err error) {
	slug := a.q.Get("slug")
	name, goals, tmpl := a.str("pname"), a.str("pgoals"), a.str("ptemplate")
	am, aq, ar := a.flag("pautomerge"), a.flag("pautoqueue"), a.flag("pautoresolve")
	_, err = h.D.Projects.Update(a.ctx, slug, projects.ProjectParams{Name: &name, Description: &goals, Template: &tmpl, AutoMerge: &am, AutoQueue: &aq, AutoResolve: &ar})
	res.msg = "saved " + slug
	return
}

func actRepoNotes(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Projects.SetNotesRepo(a.ctx, a.q.Get("slug"), a.q.Get("repo"))
	res.msg = "notes repo saved"
	return
}

func actRepoNote(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Projects.SetRepoNote(a.ctx, a.q.Get("slug"), a.q.Get("repo"), a.q.Get("note"))
	res.msg = "repo note saved"
	return
}

func actRepoLink(h *Handler, a *actReq) (res actResult, err error) {
	ref := a.str("repoadd")
	if ref == "" {
		ref = a.str("repoother")
	}
	_, err = h.D.Projects.LinkRepo(a.ctx, a.q.Get("slug"), ref, "")
	res.msg, res.reset = "repo linked", map[string]any{"repoadd": "", "repoother": ""}
	res.back = "/projects/" + a.q.Get("slug") + "/settings" // where a missing GitHub App error should link to
	return
}

func actRepoUnlink(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Projects.UnlinkRepo(a.ctx, a.q.Get("slug"), a.q.Get("repo"))
	res.msg = "repo unlinked"
	return
}

func actProjectAsk(h *Handler, a *actReq) (res actResult, err error) {
	if _, err = h.D.Agents.AskProject(a.ctx, a.q.Get("slug"), "", a.str("chatq")); err == nil {
		res.reset = map[string]any{"chatq": ""}
	}
	return
}

func actProjectProposalApply(h *Handler, a *actReq) (res actResult, err error) {
	n, _ := strconv.Atoi(a.q.Get("n"))
	if err = h.D.Agents.ApplyProject(a.ctx, a.id, n); err == nil {
		res.msg = "done"
	}
	return
}

func actProjectArchive(h *Handler, a *actReq) (res actResult, err error) {
	_, _, err = h.D.Projects.Archive(a.ctx, a.q.Get("slug"))
	var are *db.ActiveRunsError
	if errors.As(err, &are) {
		err = fmt.Errorf("%w: %s", errs.Conflict, are.Error())
	}
	res.msg, res.nav = "archived "+a.q.Get("slug"), "/projects"
	return
}

func actProjectRestore(h *Handler, a *actReq) (res actResult, err error) {
	_, err = h.D.Projects.Restore(a.ctx, a.q.Get("slug"))
	res.msg, res.nav = "restored "+a.q.Get("slug"), "/projects"
	return
}
