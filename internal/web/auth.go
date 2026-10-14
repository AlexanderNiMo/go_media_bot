package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alex/mediad/internal/storage"
)

// Cookie and context key names.
const (
	sessionCookie = "mediad_session"
	csrfCookie    = "mediad_csrf"
	csrfField     = "csrf_token"
	flashCookie   = "mediad_flash"
	flashKind     = "mediad_flash_kind"

	// flashMaxAge is how long a message survives a redirect. It only has to
	// outlive one round trip.
	flashMaxAge = 30

	// flashMaxBytes and flashMaxRunes bound what goes into the cookie. The
	// first is a byte budget well under the 4 KB cookie limit; the second
	// keeps a message of many short runes from being cut mid-character.
	flashMaxBytes = 2000
	flashMaxRunes = 400
)

// truncateRunes shortens a message on a rune boundary.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxCSRF
)

// sessionUser returns the account from the request context, or nil.
func sessionUser(r *http.Request) *storage.User {
	u, _ := r.Context().Value(ctxUser).(*storage.User)
	return u
}

// csrfToken returns the token the forms have to echo back.
func csrfToken(r *http.Request) string {
	t, _ := r.Context().Value(ctxCSRF).(string)
	return t
}

// sessions loads the account and makes sure a CSRF token exists.
//
// The token is issued on every response that does not already carry one, so a
// visitor sitting on the login page has one before they submit anything. It is
// not tied to the session: the login form needs it, and there is no session
// yet at that point.
func (s *Server) sessions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if token := s.ensureCSRF(w, r); token != "" {
			ctx = context.WithValue(ctx, ctxCSRF, token)
		}

		// Only state-changing methods are checked. A GET that changes
		// something would be a worse bug than a missing token, and requiring
		// one on every link would mean threading it through every href.
		if isUnsafe(r.Method) && !s.csrfOK(r) {
			s.log.Warn("rejected a form without a valid csrf token",
				"path", r.URL.Path, "method", r.Method, "remote", r.RemoteAddr)
			http.Error(w, "the form expired; go back and try again", http.StatusForbidden)
			return
		}

		if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
			user, err := s.store.SessionUser(ctx, cookie.Value)
			switch {
			case err == nil:
				ctx = context.WithValue(ctx, ctxUser, user)
			case errors.Is(err, storage.ErrUnauthorized):
				// Expired or revoked. Clearing it stops the browser sending a
				// dead token on every subsequent request.
				s.clearCookie(w, sessionCookie, r)
			default:
				s.log.Error("could not resolve a session", "error", err)
			}
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAuth redirects to the login page, or to the setup wizard when the
// instance has no accounts yet.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionUser(r) != nil {
			next.ServeHTTP(w, r)
			return
		}

		// Somebody arriving at a fresh instance should land in the wizard
		// rather than on a login form for an account that cannot exist yet.
		has, err := s.store.HasAccounts(r.Context())
		if err != nil {
			s.log.Error("could not tell whether the instance is set up", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !has {
			s.seeOther(w, r, "/setup")
			return
		}

		// The path is carried only as a path, never as a full URL: an open
		// redirect that takes a host from a query parameter is a phishing
		// primitive, and this is one line to avoid.
		next := url.QueryEscape(r.URL.Path)
		http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
	})
}

// safeNext returns a redirect target that is local, or the dashboard.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// ensureCSRF returns the token for this request, issuing one if needed.
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && len(cookie.Value) >= 16 {
		return cookie.Value
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// Failing to generate a token means every form submission will be
		// rejected. Saying so beats issuing a predictable one.
		s.log.Error("could not generate a csrf token", "error", err)
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isTLS(s.cfg.Server.BaseURL),
		MaxAge:   int(storage.SessionTTL.Seconds()),
	})
	return token
}

// csrfOK checks the submitted token against the cookie.
//
// Double submit: the cookie proves the browser got a token from us, and the
// form field proves the submitting page could read it. A cross-site form can
// do neither, because it cannot read our cookies.
//
// The session cookie is SameSite=Lax, which already blocks cross-site POSTs in
// every current browser. This is the layer that keeps working when that stops
// being true — an old client, a proxy that rewrites cookies, a future default
// change.
func (s *Server) csrfOK(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}

	var submitted string
	if err := r.ParseForm(); err == nil {
		submitted = r.PostFormValue(csrfField)
	}
	if submitted == "" {
		submitted = r.Header.Get("X-CSRF-Token")
	}

	return subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie.Value)) == 1
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// Login, logout, setup
// ---------------------------------------------------------------------------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if sessionUser(r) != nil {
		s.seeOther(w, r, "/")
		return
	}

	// An instance with no accounts has nothing to log into.
	has, err := s.store.HasAccounts(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !has {
		s.seeOther(w, r, "/setup")
		return
	}

	s.render(w, r, "login", struct {
		page
		Next string
	}{s.basePage(w, r, "login"), r.URL.Query().Get("next")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "login", "не разобрать форму")
		return
	}

	user, err := s.store.Authenticate(r.Context(),
		r.PostFormValue("username"), r.PostFormValue("password"))
	if err != nil {
		if !errors.Is(err, storage.ErrUnauthorized) {
			s.fail(w, r, err)
			return
		}
		s.log.Info("failed login", "username", r.PostFormValue("username"), "remote", r.RemoteAddr)

		p := s.basePage(w, r, "login")
		p.Error = "Неверное имя или пароль."
		s.render(w, r, "login", struct {
			page
			Next string
		}{p, r.PostFormValue("next")})
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}

	s.log.Info("login", "user", user.Username, "remote", r.RemoteAddr)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteSession(r.Context(), cookie.Value); err != nil {
			s.log.Error("could not delete a session on logout", "error", err)
		}
	}
	s.clearCookie(w, sessionCookie, r)
	s.seeOther(w, r, "/login")
}

// startSession issues the cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := s.store.CreateSession(r.Context(), userID)
	if err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly is what keeps the token out of reach of any script on the
		// page; it is the whole reason the token is a cookie rather than
		// something the front end holds.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isTLS(s.cfg.Server.BaseURL),
		MaxAge:   int(storage.SessionTTL.Seconds()),
	})
	return nil
}

// handleSetupForm is the first-run wizard.
func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasAccounts(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if has {
		// The wizard closes behind itself. Leaving it reachable would let
		// anyone who reaches the host create an administrator.
		s.seeOther(w, r, "/login")
		return
	}

	s.render(w, r, "setup", struct{ page }{s.basePage(w, r, "setup")})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "setup", "не разобрать форму")
		return
	}

	// Re-checked here, not only in the form handler: between rendering the
	// wizard and submitting it, somebody else may have created the first
	// account, and the second submission must not create a second
	// administrator.
	has, err := s.store.HasAccounts(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if has {
		s.seeOther(w, r, "/login")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	if problem := credentialProblem(username, password); problem != "" {
		s.renderError(w, r, "setup", problem)
		return
	}

	user, err := s.store.CreateAccount(r.Context(), username, password, true)
	if err != nil {
		s.renderError(w, r, "setup", accountProblem(err))
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}

	s.log.Info("created the first account", "user", user.Username)
	s.flash(w, r, "Аккаунт создан.", "ok")
	s.seeOther(w, r, "/")
}

// handleJoinForm is registration by invite code.
//
// Without a ?code=… query string the form asks for one. With it, the
// code is validated and the registration form is shown, or an error
// is rendered with a link back to the entry form.
func (s *Server) handleJoinForm(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		// Entry form: ask the visitor for the code first, so the link
		// from the login page does not silently throw them away.
		s.render(w, r, "join", struct {
			page
			Code        string
			InviteError string
		}{s.basePage(w, r, "join"), "", ""})
		return
	}

	// Checked before showing the form so that a dead code says so immediately
	// rather than after somebody has typed a password twice.
	inv, err := s.store.GetInvite(r.Context(), code)
	if err != nil || inv.Usable(nowFunc()) != nil {
		s.render(w, r, "join", struct {
			page
			Code        string
			InviteError string
		}{s.basePage(w, r, "join"), code, inviteProblem(err, inv)})
		return
	}

	s.render(w, r, "join", struct {
		page
		Code        string
		InviteError string
	}{s.basePage(w, r, "join"), code, ""})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "join", "не разобрать форму")
		return
	}

	code := strings.TrimSpace(r.PostFormValue("code"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	if problem := credentialProblem(username, password); problem != "" {
		s.renderJoinError(w, r, code, problem)
		return
	}

	user, err := s.store.RedeemInvite(r.Context(), code, username, password)
	if err != nil {
		s.renderJoinError(w, r, code, accountProblem(err))
		return
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}

	s.log.Info("account created from an invite", "user", user.Username)
	s.flash(w, r, "Аккаунт создан.", "ok")
	s.seeOther(w, r, "/")
}

// accountProblem explains a failure to create an account.
func accountProblem(err error) string {
	if strings.Contains(err.Error(), "the name") {
		return "Такое имя уже занято."
	}
	return "Не удалось создать аккаунт: " + err.Error()
}

func (s *Server) renderJoinError(w http.ResponseWriter, r *http.Request, code, msg string) {
	p := s.basePage(w, r, "join")
	p.Error = msg
	s.render(w, r, "join", struct {
		page
		Code string
	}{p, code})
}

// inviteProblem turns a lookup failure into something to show.
//
// The wording is here rather than in the storage layer because the same
// failures are reported in English by the CLI, and a message written once for
// both would be wrong for one of them.
func inviteProblem(err error, inv *storage.Invite) string {
	switch {
	case err != nil:
		return "Приглашение не найдено."
	case inv == nil:
		return "Приглашение недействительно."
	case inv.UsedBy != nil:
		return "Это приглашение уже использовано."
	case nowFunc().After(inv.ExpiresAt):
		return "Срок приглашения истёк."
	default:
		return "Приглашение недействительно."
	}
}

// credentialProblem explains why a name and password were refused.
//
// It checks the shared rules itself rather than printing what the storage
// layer said, so that the user reads Russian and the limits come from the one
// place they are defined.
func credentialProblem(username, password string) string {
	switch {
	case strings.TrimSpace(username) == "":
		return "Нужно имя."
	case len([]rune(strings.TrimSpace(username))) < storage.MinUsernameLength:
		return fmt.Sprintf("Имя — минимум %d символа.", storage.MinUsernameLength)
	case len(username) > storage.MaxUsernameLength:
		return fmt.Sprintf("Имя — максимум %d символов.", storage.MaxUsernameLength)
	case strings.ContainsAny(username, " \t\n"):
		return "В имени не может быть пробелов."
	case len([]rune(password)) < storage.MinPasswordLength:
		return fmt.Sprintf("Пароль — минимум %d символов.", storage.MinPasswordLength)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// basePage fills the fields the layout needs.
//
// It takes the ResponseWriter because reading the flash message and clearing
// it have to happen together: a message that is read twice is shown twice, and
// clearing it anywhere else means either losing it before the page that should
// show it or leaving it to reappear on the next navigation.
func (s *Server) basePage(w http.ResponseWriter, r *http.Request, nav string) page {
	p := page{
		User:     sessionUser(r),
		Nav:      nav,
		Version:  s.version,
		CSRF:     csrfToken(r),
		BasePath: s.cfg.Server.BasePath,
	}

	if p.User != nil {
		// Ignored on failure: the badge is decoration, and an interface that
		// refuses to render because a count query failed is worse than one
		// showing no count.
		if n, err := s.store.UnreadCount(r.Context(), p.User.ID); err == nil {
			p.Unread = n
		}
	}

	if c, err := r.Cookie(flashCookie); err == nil && c.Value != "" {
		// Decoded, because it was escaped on the way out. See flash for why
		// it cannot simply be written into the cookie.
		if msg, err := url.QueryUnescape(c.Value); err == nil {
			p.Flash = msg
		}
		if k, err := r.Cookie(flashKind); err == nil {
			p.FlashKind = k.Value
		}
		s.clearFlash(w, r)
	}

	return p
}

// clearFlash removes the flash cookies, after they have been read.
func (s *Server) clearFlash(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, flashCookie, r)
	s.clearCookie(w, flashKind, r)
}

// flash queues a message for the next page.
//
// The message is percent-escaped before it goes into the cookie, and that is
// not decoration. http.SetCookie silently drops every byte that is not a valid
// cookie-octet — which is all non-ASCII, without any error — so a message like
// "Код приглашения: EpyzfRNpzEvH" arrives as " : EpyzfRNpzEvH". The same would
// happen to any title in any confirmation, and the failure is invisible until
// somebody reads the mangled text.
//
// It is also truncated. A cookie is capped at about 4 KB and an over-long one
// is dropped by the browser entirely, which would lose the message rather than
// shorten it.
func (s *Server) flash(w http.ResponseWriter, r *http.Request, msg, kind string) {
	if len(msg) > flashMaxBytes {
		msg = truncateRunes(msg, flashMaxRunes)
	}

	secure := isTLS(s.cfg.Server.BaseURL)
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(msg), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure,
		MaxAge: flashMaxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name: flashKind, Value: kind, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure,
		MaxAge: flashMaxAge,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: isTLS(s.cfg.Server.BaseURL),
		MaxAge: -1,
	})
}

// renderError renders a page with an error message on it.
//
// For a form that failed, this is the better answer than a redirect: the user
// keeps what they typed, and the message arrives next to the fields rather
// than on a page they have to navigate back from.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, name, msg string) {
	p := s.basePage(w, r, "")
	p.Error = msg
	w.WriteHeader(http.StatusOK)
	s.render(w, r, name, struct{ page }{p})
}

// fail logs an unexpected error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ---------------------------------------------------------------------------
// Address helpers
// ---------------------------------------------------------------------------

func splitHostPort(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	return host, port, nil
}

func isLoopback(host string) bool {
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func isTLS(baseURL string) bool {
	return strings.HasPrefix(strings.ToLower(baseURL), "https://")
}

// nowFunc exists so tests can move the clock without a build tag.
var nowFunc = func() time.Time { return time.Now() }
