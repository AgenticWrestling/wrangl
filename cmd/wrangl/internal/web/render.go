package web

import (
	"bytes"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/projects"
)

var (
	gm     = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(gmhtml.WithHardWraps()))
	policy = func() *bluemonday.Policy {
		p := bluemonday.UGCPolicy()
		p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "pre", "span", "div")
		p.AllowAttrs("type", "checked", "disabled").OnElements("input") // GFM task lists
		return p
	}()
)

// markdown renders GitHub-flavoured markdown, sanitised (agent output is untrusted).
func markdown(s string) template.HTML {
	var b bytes.Buffer
	if err := gm.Convert([]byte(s), &b); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(s) + "</pre>")
	}
	return template.HTML(policy.SanitizeBytes(b.Bytes()))
}

const maxHighlight = 512 << 10

var formatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(true), chromahtml.LineNumbersInTable(false))

// highlight renders file content with chroma classes (styled by /static/chroma.css).
func highlight(path, content string) template.HTML {
	if len(content) > maxHighlight {
		return template.HTML("<pre class=\"chroma\">" + template.HTMLEscapeString(content) + "</pre>")
	}
	lexer := lexers.Match(path)
	if lexer == nil {
		lexer = lexers.Analyse(content)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, content)
	if err != nil {
		return template.HTML("<pre class=\"chroma\">" + template.HTMLEscapeString(content) + "</pre>")
	}
	var b bytes.Buffer
	if err := formatter.Format(&b, styles.Get("github"), it); err != nil {
		return template.HTML("<pre class=\"chroma\">" + template.HTMLEscapeString(content) + "</pre>")
	}
	return template.HTML(b.String())
}

// chromaCSS is the light style plus a dark one under prefers-color-scheme.
func chromaCSS() string {
	var light, dark bytes.Buffer
	formatter.WriteCSS(&light, styles.Get("github"))
	formatter.WriteCSS(&dark, styles.Get("github-dark"))
	return light.String() + "\n@media (prefers-color-scheme: dark) {\n" + dark.String() + "\n}\n"
}

var (
	iconMu    sync.Mutex
	iconCache = map[int][]byte{}
)

// icon serves /icon-<size>.png: a generated PNG (installable PWAs on iOS want raster icons).
func (h *Handler) icon(w http.ResponseWriter, r *http.Request) {
	size, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("size"), ".png"))
	if err != nil || size < 16 || size > 1024 {
		http.NotFound(w, r)
		return
	}
	iconMu.Lock()
	b, ok := iconCache[size]
	if !ok {
		b = drawIcon(size)
		iconCache[size] = b
	}
	iconMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

// drawIcon draws a lasso-ish ring with a dot on the brand colour.
func drawIcon(n int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	bg := color.RGBA{0x1f, 0x6f, 0x5c, 0xff}
	fg := color.RGBA{0xf4, 0xf1, 0xe8, 0xff}
	c := float64(n) / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			d2 := dx*dx + dy*dy
			r := float64(n)
			switch {
			case d2 > (0.22*r)*(0.22*r) && d2 < (0.30*r)*(0.30*r):
				img.Set(x, y, fg)
			case d2 < (0.07*r)*(0.07*r):
				img.Set(x, y, fg)
			default:
				img.Set(x, y, bg)
			}
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// projectsParams builds create params from the form (template "" = none).
func projectsParams(template string) projects.ProjectParams {
	p := projects.ProjectParams{}
	if template != "" {
		p.Template = &template
	}
	return p
}
