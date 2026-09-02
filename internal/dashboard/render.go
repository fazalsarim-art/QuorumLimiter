// Package dashboard renders the server-side admin interface: embedded
// html/template pages, static assets, session sign-in, and CSRF-protected form
// mutations. Templates receive prepared view models and never touch storage.
package dashboard

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"strings"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// pageNames are the content templates composed with the base layout.
var pageNames = []string{"login", "overview", "policies", "clients", "decisions", "cluster", "docs"}

// flash is a one-shot notice shown after a redirect.
type flash struct {
	Level   string // "success", "error", "info"
	Message string
}

// topBar is the header context shown on every authenticated page.
type topBar struct {
	NodeID   string
	Role     string
	Term     uint64
	LeaderID string
}

// pageData is the common template model. Data holds page-specific content.
type pageData struct {
	Title     string
	Nav       string // active nav key
	Authed    bool
	CSRFToken string
	TopBar    topBar
	Flash     *flash
	Data      any
}

type renderer struct {
	pages map[string]*template.Template
}

func newRenderer() (*renderer, error) {
	funcs := template.FuncMap{
		"msTime": func(ms int64) string {
			if ms == 0 {
				return "—"
			}
			return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05Z")
		},
		"tokens": func(milli int64) string {
			whole := milli / 1000
			frac := milli % 1000
			if frac == 0 {
				return itoa(whole)
			}
			return itoa(whole) + "." + pad3(frac)
		},
		"join": strings.Join,
	}
	r := &renderer{pages: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		t, err := template.New("base.html").Funcs(funcs).ParseFS(templatesFS,
			"templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		r.pages[name] = t
	}
	return r, nil
}

// render writes a page. It renders into a buffer first so a template error never
// produces a partially written response.
func (rd *renderer) render(w http.ResponseWriter, status int, page string, data pageData) {
	t, ok := rd.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func pad3(v int64) string {
	s := itoa(v)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
