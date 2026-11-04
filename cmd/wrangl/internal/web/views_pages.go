package web

// Template data for Inbox, project, GitHub setup, template and Settings.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/ghsetup"
)

func (h *Handler) viewInbox(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	qs, err := h.D.Runs.Questions(ctx, false)
	if err != nil {
		return err
	}
	prs, _ := db.RQ.ListPRs(ctx, "open")
	d["OpenQuestions"] = qs // reused for the sidebar badge
	d["Title"] = "Inbox"
	d["Items"] = h.inboxItems(ctx, qs, prs, h.changeLinks(ctx, 0))
	return nil
}

func (h *Handler) viewProject(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	p, err := h.D.Projects.Project(ctx, reqValue(r, "slug"))
	if err != nil {
		return err
	}
	repos, err := db.RQ.ListRepos(ctx, p.ID)
	if err != nil {
		return err
	}
	d["P"], d["Repos"], d["Title"] = p, repos, p.Name
	d["NotesRepo"], _ = db.RQ.GetNotesRepoID(ctx, p.ID)
	d["Templates"], _ = db.RQ.ListTemplates(ctx)
	d["Limits"], _ = h.D.Projects.Limits(ctx, p.Slug)
	d["GitHub"] = h.D.Projects.GH != nil
	d["Pick"] = h.repoChoices(ctx, p.Slug, repos)
	d["AutoResolve"] = false
	if v, _ := db.RQ.GetProjectAutoResolve(ctx, p.ID); v == 1 {
		d["AutoResolve"] = true
	}
	d["Template"] = ""
	if p.DefaultTemplateID.Valid {
		if t, err := db.RQ.GetTemplate(ctx, p.DefaultTemplateID.Int64); err == nil {
			d["Template"] = t.Name
		}
	}
	return nil
}

func (h *Handler) viewSetup(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	d["Title"] = "GitHub accounts"
	qv := r.URL.Query()
	back := ghsetup.SafeBack(qv.Get("back"))
	accts, _ := h.D.Setup.Accounts(ctx)
	var views []acctView
	seen := false
	for _, ac := range accts {
		v := acctView{Account: ac, AppName: ghsetup.AppName(ac.Login)}
		if ac.App != nil { // re-authorizing makes a new App: names are unique across GitHub
			v.AppName = ghsetup.FreshAppName(ac.Login)
		}
		v.State = h.D.Setup.NewState(ac.Login, back)
		m, err := h.D.Setup.Manifest(v.AppName)
		v.Manifest, v.ManifestErr = string(m), errString(err)
		v.Status, v.StatusText, v.NRepos = h.appHealth(ctx, ac)
		v.Auto = ac.App == nil && strings.EqualFold(qv.Get("create"), ac.Login) || ac.App == nil && qv.Get("create") == "1" && strings.EqualFold(qv.Get("account"), ac.Login)
		v.JustCreated = ac.App != nil && qv.Get("role") != "reviewer" && strings.EqualFold(qv.Get("created"), ac.Login) && v.Status == "noinstall"
		if ac.Reviewer == nil {
			v.RName = ghsetup.ReviewerName(ac.Login)
			v.RState = h.D.Setup.NewRoleState(ac.Login, back, "reviewer")
			m, err := h.D.Setup.ManifestFor(v.RName, "reviewer")
			v.RManifest, v.RManifestErr = string(m), errString(err)
		}
		v.Focus = strings.EqualFold(qv.Get("account"), ac.Login)
		seen = seen || v.Focus
		views = append(views, v)
	}
	if h.D.Secrets != nil {
		defs, _ := db.RQ.ListAgentDefs(ctx)
		d["Creds"] = agentdef.Credentials(defs, h.D.Secrets.Has)
	}
	d["Accounts"], d["Created"], d["Error"] = views, qv.Get("created"), qv.Get("error")
	d["CreatedRole"] = qv.Get("role")
	d["Repo"], d["Back"] = qv.Get("repo"), back
	if f := qv.Get("account"); f != "" && !seen {
		d["Suggest"] = f // an owner we could not add (GitHub did not know it): prefill the add box
	}
	return nil
}

func (h *Handler) viewTemplate(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	t, err := h.D.Fleet.Template(ctx, reqValue(r, "name"))
	if err != nil {
		return err
	}
	d["T"], d["Title"] = t, "Template "+t.Name
	var types []string
	json.Unmarshal([]byte(t.InstanceTypes), &types)
	ticked := map[string]bool{}
	for _, x := range types {
		ticked[x] = true
	}
	d["Ticked"], d["TypesList"], d["TypesCSV"] = ticked, types, strings.Join(types, ",")
	d["SessStr"] = strconv.FormatInt(t.MaxSessions, 10)
	d["MaxStr"], d["IdleStr"] = strconv.FormatInt(t.MaxRunners, 10), shortDuration(time.Duration(t.IdleTimeoutS)*time.Second)
	d["VcpuStr"], d["RamStr"], d["DiskStr"] = strconv.FormatInt(t.MinVcpu, 10), fmt.Sprintf("%gGiB", float64(t.MinRamMib)/1024), fmt.Sprintf("%dGiB", t.MinDiskGib)
	d["ImgMode"], d["AmiStr"] = "al", ""
	if strings.HasPrefix(t.Ami, "ami-") {
		d["ImgMode"], d["AmiStr"] = "ami", t.Ami
	}
	var agent struct {
		Start []string `json:"start"`
	}
	json.Unmarshal([]byte(t.AgentJson), &agent)
	d["AgentSummary"] = "no agent"
	if len(agent.Start) > 0 {
		d["AgentSummary"] = "runs " + agent.Start[0]
	}
	d["HasAWS"] = h.D.Fleet.Prov != nil
	if h.D.Costs != nil {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cat, err := h.D.Costs.Catalogue(cctx)
		cancel()
		d["Catalogue"], d["CatalogueErr"], d["Region"] = cat, errString(err), h.D.Costs.Home
	}
	return nil
}

func (h *Handler) viewSettings(ctx context.Context, view string, r *http.Request, d map[string]any) error {
	db := h.D.Fleet.DB
	d["GHAccounts"], _ = h.D.Setup.Accounts(ctx)
	d["Projects"], _ = db.RQ.ListProjects(ctx, "active")
	d["ArchivedProjects"], _ = db.RQ.ListProjects(ctx, "archived")
	d["Templates"], _ = db.RQ.ListTemplates(ctx)
	d["CAgents"], _ = db.RQ.ListControllerAgents(ctx)
	d["VAPID"], d["Title"] = h.D.Push.Keys.Public, "Settings"
	return nil
}
