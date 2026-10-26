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
	"io"
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
	"help",
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
		// path joins a route to the deployment's base path. A leading slash
		// on the route is required; a missing one is left as-is so a typo
		// surfaces in the rendered page rather than silently rewriting URLs.
		"path":      pathFunc,
		"hasString": hasString,
		// bytes renders a file size the way a person reads it. The raw
		// byte count is what the database holds and what a script wants;
		// the page is read by people.
		"bytes": humanBytes,
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
	// FlashHTML, when set, replaces Flash and is rendered verbatim. Used
	// for messages that must carry a link or other inline markup, where
	// the plain-text Flash would lose the format.
	FlashHTML string
	Error     string
	Version   string
	// CSRF is what every form has to echo back. It is on the layout's data
	// rather than passed per form because a form that forgets it is rejected
	// at submission time, which is a confusing way to find out.
	CSRF string
	// BasePath is the URL prefix the interface is mounted under. Templates
	// use the path helper rather than reading this directly.
	BasePath string
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

// pathFunc joins a base path and a route.
//
// Both arguments are expected to start with "/". The route's leading slash is
// kept; the base path's trailing slash is stripped, so "/mediad" + "/login"
// becomes "/mediad/login" rather than "/mediad//login".
//
// An empty basePath is allowed: it means the interface is at the host root
// and the route is returned unchanged.
func pathFunc(basePath, route string) string {
	if basePath == "" {
		return route
	}
	return strings.TrimRight(basePath, "/") + route
}

// hasString reports whether v (any printable type) appears in slice.
// state.State is a defined string type, so the caller passes a State value
// directly and we stringify it; this is what lets a template iterate
// []storage.State and ask whether each one is in a []string of filter picks.
func hasString(slice []string, v any) bool {
	s := fmt.Sprint(v)
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}

// path is the Go-side equivalent of the template helper.
func (s *Server) path(route string) string {
	return pathFunc(s.cfg.Server.BasePath, route)
}

// seeOther is the project's redirect-after-POST helper.
//
// It takes a route relative to the host root, prepends the deployment's base
// path, and writes a 303. The 303 is what makes a POST-POST-PRG cycle safe:
// the browser follows the redirect with GET rather than replaying the form.
func (s *Server) seeOther(w http.ResponseWriter, r *http.Request, route string) {
	http.Redirect(w, r, s.path(route), http.StatusSeeOther)
}

// handleFavicon serves the embedded SVG icon.
//
// Browsers default to /favicon.ico on every page; without this handler
// the request 404s and the server logs it on every render. The content
// type is image/svg+xml, and a long cache lifetime is safe because the
// icon is versioned in the binary and only changes on a release.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	f, err := s.tmpl.dirFS.Open("static/favicon.svg")
	if err != nil {
		// Unreachable in practice: the embed directive guarantees the file.
		http.Error(w, "favicon not embedded", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("favicon copy failed", "error", err)
	}
}
