package web

// Actions: actions on GitHub accounts and controller agents.

import (
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"net/url"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/ghsetup"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func actAccountCheck(h *Handler, a *actReq) (res actResult, err error) {
	h.health.Delete(strings.ToLower(a.q.Get("login")))
	res.msg, res.nav = "checking…", "/setup?account="+url.QueryEscape(a.q.Get("login"))
	return
}

func actAgentSave(h *Handler, a *actReq) (res actResult, err error) {
	ag := dbgen.ControllerAgent{Name: a.str("agname"), Command: a.str("agcmd"), Model: a.str("agmodel"), Auth: a.str("agauth"), AuthSecret: a.str("agsecret"), AuthEnv: a.str("agenv"), Enabled: 1}
	ag.MaxTurns, _ = strconv.ParseInt(a.str("agturns"), 10, 64)
	ag.TimeoutS, _ = strconv.ParseInt(a.str("agtimeout"), 10, 64)
	if err = h.D.Agents.Save(a.ctx, ag, a.flag("agdefault")); err == nil {
		res.msg = "saved " + ag.Name
	}
	return
}

func actAgentDelete(h *Handler, a *actReq) (res actResult, err error) {
	if err = h.D.Agents.Delete(a.ctx, a.q.Get("name")); err == nil {
		res.msg = "removed " + a.q.Get("name")
	}
	return
}

func actAccountAdd(h *Handler, a *actReq) (res actResult, err error) {
	_, err = h.D.Setup.AddAccount(a.ctx, a.str("acctlogin"), a.str("acctkind"))
	res.msg, res.reset = "account added", map[string]any{"acctlogin": ""}
	// No App yet means create it now: the setup page goes straight on to GitHub.
	res.nav = "/setup?account=" + url.QueryEscape(strings.TrimPrefix(a.str("acctlogin"), "@")) + "&create=1&back=" + url.QueryEscape(ghsetup.SafeBack(a.q.Get("back")))
	return
}

func actAccountRemove(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Setup.RemoveAccount(a.ctx, a.q.Get("login"))
	res.msg = "account removed"
	res.nav = "/setup"
	return
}

// placementInput is a stored need as the Settings form shows it ("" while it is the unset 0).
func placementInput(field string, v int64) string {
	if v == 0 {
		return ""
	}
	switch field {
	case "vcpu":
		return strconv.FormatInt(v, 10)
	case "ram":
		if v%1024 == 0 {
			return strconv.FormatInt(v/1024, 10) + "GiB"
		}
		return strconv.FormatInt(v, 10) + "MiB"
	}
	return strconv.FormatInt(v, 10) + "GiB"
}

func actPlacementSave(h *Handler, a *actReq) (res actResult, err error) {
	patch := map[string]string{}
	for field, key := range map[string]string{"vcpu": "plvcpu", "ram": "plram", "disk": "pldisk"} {
		if v := a.str(key); v != "" {
			patch[field] = v
		}
	}
	if _, err = (&placement.Service{DB: h.D.Fleet.DB}).SetGlobal(a.ctx, patch); err == nil {
		res.msg = "default needs saved"
	}
	return
}
