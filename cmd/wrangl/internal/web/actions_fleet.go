package web

// Actions: actions on runners, instances and templates.

import (
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"html/template"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func actInstanceTerminate(h *Handler, a *actReq) (res actResult, err error) {
	var name string
	name, err = api.TerminateOther(a.ctx, h.D, a.q.Get("region"), a.q.Get("iid"))
	res.msg = "terminating " + name
	return
}

func actInstanceAdopt(h *Handler, a *actReq) (res actResult, err error) {
	var ad *api.AdoptResult
	ad, err = api.Adopt(a.ctx, h.D, a.q.Get("region"), a.q.Get("iid"), a.str("adopttpl"))
	if err == nil {
		note := ""
		if !ad.Reachable {
			note = `<p class="warn">` + template.HTMLEscapeString(ad.Note) + `</p>`
		}
		mergeFragments(a.w, `<div a.id="adopt-result" class="adopt-result"><p>Run this on the instance (it needs the wrangl binary, from <code>`+template.HTMLEscapeString(ad.Binary)+`</code>, and the token works once for 30 minutes):</p><pre>`+template.HTMLEscapeString(ad.Command)+`</pre>`+note+`</div>`)
		res.msg = "token created"
	}
	return
}

func actRunner(h *Handler, a *actReq) (res actResult, err error) {
	name := a.q.Get("name")
	switch a.q.Get("op") {
	case "cordon":
		err = h.D.Fleet.Cordon(a.ctx, name, true)
	case "uncordon":
		err = h.D.Fleet.Cordon(a.ctx, name, false)
	case "drain":
		err = h.D.Fleet.Drain(a.ctx, name)
	case "terminate":
		err = h.D.Fleet.Terminate(a.ctx, name)
	default:
		err = fmt.Errorf("%w: unknown runner op %q", errs.Invalid, a.q.Get("op"))
	}
	res.msg = name + ": " + a.q.Get("op")
	return
}

func actFleetLaunch(h *Handler, a *actReq) (res actResult, err error) {
	_, err = h.D.Fleet.Launch(a.ctx, a.q.Get("template"))
	res.msg = "launching a " + a.q.Get("template") + " runner"
	return
}

func actTemplateSave(h *Handler, a *actReq) (res actResult, err error) {
	var t dbgen.RunnerTemplate
	t, err = h.D.Fleet.Template(a.ctx, a.q.Get("name"))
	if err == nil {
		var body []byte
		if body, err = templateYAMLFromForm(t, a.str); err == nil {
			var nt dbgen.RunnerTemplate
			if nt, err = h.D.Fleet.UpdateTemplate(a.ctx, t.Name, body); err == nil {
				res.msg = fmt.Sprintf("%s is now v%d", nt.Name, nt.Version)
				res.nav = "/settings"
			}
		}
	}
	return
}
