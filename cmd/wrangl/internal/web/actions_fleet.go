package web

// Actions: actions on runners, instances and templates.

import (
	"encoding/json"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"html/template"
	"net/http"
	"regexp"
	"strings"

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
		mergeFragments(a.w, `<div id="adopt-result" class="adopt-result"><p>Run this on the instance (it needs the wrangl binary, from <code>`+template.HTMLEscapeString(ad.Binary)+`</code>, and the token works once for 30 minutes):</p><pre>`+template.HTMLEscapeString(ad.Command)+`</pre>`+note+`</div>`)
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

// --- runner template editor: image checks (the form state itself lives in signals) --------------

var amiRe = regexp.MustCompile(`^ami-[0-9a-f]+$`)

func esc(s string) string { return template.HTMLEscapeString(s) }

// flushed sends html as a patch and pushes it to the browser now (a slow action shows progress).
func flushed(a *actReq, html string) {
	a.w.Header().Set("Content-Type", "text/event-stream")
	mergeFragments(a.w, html)
	if f, ok := a.w.(http.Flusher); ok {
		f.Flush()
	}
}

func actTplAmiCheck(h *Handler, a *actReq) (res actResult, err error) {
	res.quiet = true
	ami, typ := a.str("tpami"), a.q.Get("type")
	if !amiRe.MatchString(ami) || typ == "" || h.D.Fleet.Prov == nil {
		flushed(a, `<div id="tp-ami-check" class="imgcheck"></div>`)
		return
	}
	c := h.D.Fleet.Prov.CheckLaunch(a.ctx, ami, typ, h.D.Fleet.SubnetID, h.D.Fleet.SecurityGroupID)
	var out string
	switch {
	case c.OK:
		out = `<span class="ok">✓ EC2 says it can launch this image on ` + esc(typ) + `</span>`
	case c.OptInRequired:
		out = `<span class="warn">! Not subscribed.</span> Accept the vendor's terms in the AWS console first`
		if strings.HasPrefix(c.SubscribeURL, "https://") {
			out += `: <a href="` + esc(c.SubscribeURL) + `" target="_blank" rel="noopener">open the Marketplace page</a>`
		}
		out += "."
	default:
		out = `<span class="warn">! ` + esc(c.Error) + `</span>`
	}
	flushed(a, `<div id="tp-ami-check" class="imgcheck">`+out+`</div>`)
	return
}

func actTplAmiTest(h *Handler, a *actReq) (res actResult, err error) {
	res.quiet = true
	ami, typ, arch := a.str("tpami"), a.q.Get("type"), a.q.Get("arch")
	say := func(s string) { flushed(a, `<ul id="tp-ami-test-out" class="imgcheck" aria-live="polite">`+s+`</ul>`) }
	if !amiRe.MatchString(ami) {
		say("Enter an AMI id first.")
		return
	}
	if typ == "" || (arch != "arm64" && arch != "x86_64") {
		say("Pick an instance type first.")
		return
	}
	say("Launching " + esc(typ) + " and waiting for it to report (about 2 minutes)…")
	r, terr := h.D.Fleet.TestImage(a.ctx, ami, typ, arch)
	if terr != nil {
		say("Test failed: " + esc(terr.Error()))
		return
	}
	var b strings.Builder
	for _, k := range r.Checks {
		mark := `<span class="warn">✗</span>`
		if k.OK {
			mark = `<span class="ok">✓</span>`
		}
		b.WriteString("<li>" + mark + " " + esc(k.Name))
		if k.Detail != "" {
			b.WriteString(` <span class="dim small">` + esc(k.Detail) + `</span>`)
		}
		b.WriteString("</li>")
	}
	if r.Terminated {
		b.WriteString(`<li class="dim small">Test instance terminated.</li>`)
	} else {
		b.WriteString(`<li class="dim small">Could not terminate the test instance ` + esc(r.InstanceID) + `; the reaper will remove it.</li>`)
	}
	say(b.String())
	return
}

func actTplMarket(h *Handler, a *actReq) (res actResult, err error) {
	res.quiet = true
	q, arch := a.str("tpq"), a.q.Get("arch")
	show := func(s string) { flushed(a, `<div id="tp-mres" class="mres">`+s+`</div>`) }
	if h.D.Fleet.Prov == nil {
		show("AWS is not configured on this controller.")
		return
	}
	imgs, serr := h.D.Fleet.Prov.SearchMarketplace(a.ctx, q, arch)
	if serr != nil {
		show(esc(serr.Error()))
		return
	}
	var b strings.Builder
	for _, im := range imgs {
		use := fmt.Sprintf("$tpimg = 'ami'; $tpami = %s; $tparch = %s; @post('/ui/act/tpl-ami-check?type=' + encodeURIComponent($tpcheap?.t || ''))", jsStr(im.ID), jsStr(im.Arch))
		fmt.Fprintf(&b, `<div class="row mrow"><span class="grow"><b>%s</b> <span class="dim small">%s</span></span><code class="mono small">%s</code><button type="button" class="btn small" data-on:click="%s">Use</button></div>`,
			esc(im.Name), esc(im.Arch), esc(im.ID), esc(use))
	}
	if b.Len() == 0 {
		show(`No Marketplace images for "` + esc(q) + `" on ` + esc(arch) + `.`)
		return
	}
	show(b.String())
	return
}

// jsStr quotes s as a JavaScript string literal for use inside an expression.
func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }
