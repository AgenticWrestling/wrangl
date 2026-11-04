package web

// Actions: actions on projects and their repos.

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
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
	res.msg, res.nav = "project created", "/project/"+url.PathEscape(slug)
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

func actProjectLimit(h *Handler, a *actReq) (res actResult, err error) {
	var n int64
	if n, err = strconv.ParseInt(a.q.Get("n"), 10, 64); err != nil || n < 0 {
		err = fmt.Errorf("limit must be a whole number, 0 or more")
		return
	}
	err = h.D.Projects.SetLimits(a.ctx, a.q.Get("slug"), map[string]int64{a.q.Get("template"): n})
	res.msg = fmt.Sprintf("%s limit on %s: %d", a.q.Get("slug"), a.q.Get("template"), n)
	return
}

func actRepoNotes(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Projects.SetNotesRepo(a.ctx, a.q.Get("slug"), a.q.Get("repo"))
	res.msg = "notes repo saved"
	return
}

func actRepoLink(h *Handler, a *actReq) (res actResult, err error) {
	ref := a.str("repoadd")
	if ref == "" {
		ref = a.str("repoother")
	}
	_, err = h.D.Projects.LinkRepo(a.ctx, a.q.Get("slug"), ref, "")
	res.msg, res.reset = "repo linked", map[string]any{"repoadd": "", "repoother": ""}
	res.back = "/project/" + a.q.Get("slug") // where a missing GitHub App error should link to
	return
}

func actRepoUnlink(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Projects.UnlinkRepo(a.ctx, a.q.Get("slug"), a.q.Get("repo"))
	res.msg = "repo unlinked"
	return
}
