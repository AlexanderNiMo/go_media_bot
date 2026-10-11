// Package web is the HTTP front door: the server-rendered interface and the
// form handlers behind it.
//
// It renders from Postgres and never from Temporal. That is not an
// optimisation. SQL-backed visibility has no ORDER BY at all — asking for one
// is a hard InvalidArgument — it is eventually consistent by seconds, and a
// workflow query needs a live worker polling the queue, so an interface built
// on it shows a page of nothing whenever the worker is down or restarting.
// The projection exists so that the render path cannot be affected by either.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alex/mediad/internal/storage"
)

//go:embed views
var viewsFS embed.FS

// The page templates. Each is parsed together with the layout and the shared
// fragments, and rendered into a buffer before anything is written to the
// response — see render for why that matters.
var pageNames = []string{
	"dashboard",
	"search",
	"seasons",
	"confirm",
	"requests",
	"request",
	"notifications",
	"settings",
	"login",
	"setup",
	"join",
}

/*
templates holds one parsed set per page.

A single set containing every page does not work: html/template resolves
{{template "content"}} at execution time, and all eleven pages define "content",
so the last one parsed would win and every page would render the same body.
Parsing layout + fragments + one page per set keeps the names unambiguous and
the failure impossible.
*/
type templates struct {
	set   map[string]*template.Template
	dirFS fs.FS
}

func newTemplates() (*templates, error) {
	funcs := template.FuncMap{
		"percent": percent,
		"since":   since,
	}

	t := &templates{set: make(map[string]*template.Template, len(pageNames))}

	for _, name := range pageNames {
		parsed, err := template.New(name).Funcs(funcs).ParseFS(viewsFS,
			"views/layout.html", "views/fragments.html", "views/"+name+".html")
		if err != nil {
			// At startup rather than at first render: a template that does not
			// parse is a broken deployment, and finding out from a blank page
			// in a browser is a worse way to learn it.
			return nil, fmt.Errorf("parse the %s template: %w", name, err)
		}
		t.set[name] = parsed
	}

	sub, err := fs.Sub(viewsFS, "views")
	if err != nil {
		return nil, fmt.Errorf("open the views directory: %w", err)
	}
	t.dirFS = sub

	return t, nil
}

// page is what every template needs from the layout.
//
// Embedded in each view model rather than passed alongside, so that a handler
// returns one value and the layout's fields are at the top level where the
// template expects them.
type page struct {
	User      *storage.User
	Nav       string
	Unread    int
	Flash     string
	FlashKind string
	Error     string
	Version   string
	// CSRF is what every form has to echo back. It is on the layout's data
	// rather than passed per form because a form that forgets it is rejected
	// at submission time, which is a confusing way to find out.
	CSRF string
}

// render writes a page.
//
// The template is executed into a buffer first, and that is the point of the
// function. A template that fails halfway through — a nil field it did not
// expect — would otherwise have already written its headers and part of its
// body, so the response would be a 200 carrying half a page. Buffering turns
// that into a clean 500 and one log line.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, ok := s.tmpl.set[name]
	if !ok {
		s.log.Error("no such template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("rendering a page failed", "template", name, "error", err,
			"path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderFragment writes a partial, with no layout.
//
// It is executed through the dashboard set, because that is where the
// fragment's own template happens to live; the fragment itself refers to
// nothing page-specific, and the buffer rule above applies here too.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, ok := s.tmpl.set["dashboard"]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("rendering a fragment failed", "fragment", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// staticHandler serves the embedded CSS and JavaScript.
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(s.tmpl.dirFS, "static")
	if err != nil {
		// Unreachable: dirFS is "views" and the embed directive guarantees the
		// subdirectory. Failing loudly beats serving a directory listing.
		panic(fmt.Sprintf("web: static files are missing from the embedded views: %v", err))
	}

	// StripPrefix, or every asset 404s. The router mounts this handler at
	// "/static/*" and passes the path through unchanged, so without it the
	// file server looks for "static/style.css" inside a tree already rooted at
	// the static directory.
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

// percent renders a 0..1 fraction for a style attribute.
//
// It is clamped, and it trims to one decimal: the value arrives from a torrent
// client and a wild one — a client reporting 1.2 during a recheck — would
// otherwise draw a bar past the end of its container.
func percent(f float64) string {
	switch {
	case f <= 0:
		return "0"
	case f >= 1:
		return "100"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", f*100), ".0")
	}
}

// since renders a timestamp as how long ago it was.
//
// Relative rather than absolute for anything recent, because the question a
// list answers is "is this moving", and "3m ago" answers it at a glance where
// "01:42:17" needs arithmetic against the clock.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

// logRequest is a one-line access log.
//
// No middleware library for three fields, and no response wrapping: the status
// code is the thing worth having in a log for a household tool, and getting it
// means intercepting WriteHeader for the sake of one column.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		if r.URL.Path == "/fragments/active" {
			// The dashboard polls this every three seconds. Logging it would
			// bury everything else.
			return
		}

		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"took", time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// logger returns a logger tagged for this package.
func logger(base *slog.Logger) *slog.Logger {
	return base.With("component", "web")
}
