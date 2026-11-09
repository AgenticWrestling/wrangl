package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// The review card's "Changes" section: a collapsed <details> that loads GET /ui/changes on first
// open (data-on:toggle) and patches the result into #chg-<key>, which is data-ignore-morph so live
// re-renders of the card leave the loaded diff, and which files are expanded, alone.

type chgLine struct{ Class, Text string }

type changesFile struct {
	proto.DiffFile
	Lines []chgLine
}

type changesData struct {
	proto.DiffView
	Files   []changesFile
	Base    string // the /ui/changes URL without ?commit
	DOM     string
	Err     string
	Adds    int
	Dels    int
	Current string
}

// changesURL is the lazy-load URL for a card, "" when the run has no code to show.
func changesURL(run, pr int64) string {
	if pr != 0 {
		return fmt.Sprintf("/ui/changes?pr=%d", pr)
	}
	if run != 0 {
		return fmt.Sprintf("/ui/changes?run=%d", run)
	}
	return ""
}

func chgLines(patch string) []chgLine {
	var out []chgLine
	in := false
	for _, l := range strings.Split(patch, "\n") {
		if strings.HasPrefix(l, "@@") {
			in = true
			out = append(out, chgLine{"d-hunk", l})
			continue
		}
		if !in || l == "" {
			continue
		}
		switch l[0] {
		case '+':
			out = append(out, chgLine{"d-add", l[1:]})
		case '-':
			out = append(out, chgLine{"d-del", l[1:]})
		case '\\':
			out = append(out, chgLine{"d-hunk", l})
		default:
			out = append(out, chgLine{"d-ctx", strings.TrimPrefix(l, " ")})
		}
	}
	return out
}

// changes serves GET /ui/changes?pr=ID|run=ID[&commit=SHA]: the section body as a Datastar patch.
func (h *Handler) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pr, _ := strconv.ParseInt(q.Get("pr"), 10, 64)
	run, _ := strconv.ParseInt(q.Get("run"), 10, 64)
	key := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q.Get("k"))
	d := changesData{Base: changesURL(run, pr) + "&k=" + key, DOM: "chg-" + key}
	var v proto.DiffView
	var err error
	switch {
	case h.D.Diffs == nil:
		err = fmt.Errorf("diffs are not available")
	case pr != 0:
		v, err = h.D.Diffs.ForPR(r.Context(), pr, q.Get("commit"))
	case run != 0:
		v, err = h.D.Diffs.ForRun(r.Context(), run, q.Get("commit"))
	default:
		err = fmt.Errorf("pr or run required")
	}
	if err != nil {
		d.Err = err.Error()
	}
	d.DiffView = v
	d.Current = v.Commit
	for _, f := range v.Files {
		d.Adds += f.Additions
		d.Dels += f.Deletions
		d.Files = append(d.Files, changesFile{DiffFile: f, Lines: chgLines(f.Patch)})
	}
	body, rerr := h.render("changes-body", d)
	if rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, "event: datastar-patch-elements\ndata: selector #%s\ndata: mode inner\n", d.DOM)
	for _, line := range strings.Split(string(body), "\n") {
		fmt.Fprintf(w, "data: elements %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
