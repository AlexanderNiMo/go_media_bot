package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/storage"
)

// The handler tests run the real mux against a real database.
//
// What they cover that the template tests cannot: that a route is reachable,
// that the session middleware admits the right requests and redirects the
// rest, that a form's fields are read under the names the template wrote, and
// that a POST redirects rather than rendering. Those are the failures that
// look fine in a unit test and wrong in a browser.
//
// They need Postgres, and skip without it, exactly as the storage tests do.

const testDSNEnv = "MEDIAD_TEST_DSN"

func newTestServer(t *testing.T) (*Server, *storage.Store) {
	t.Helper()

	adminDSN := os.Getenv(testDSNEnv)
	if adminDSN == "" {
		t.Skipf("%s is not set; run `make up` and export it to run web tests", testDSNEnv)
	}

	ctx := context.Background()
	name := fmt.Sprintf("mediad_web_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	defer admin.Close(ctx)

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create the test database: %v", err)
	}

	// The database is set on the parsed config rather than by rewriting the
	// DSN: pgx's ConnConfig.ConnString() returns the string it was built from,
	// so an edited cfg would silently point every test at the first database.
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parse the dsn: %v", err)
	}
	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropDatabase(adminDSN, name)
	})

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := storage.NewStore(pool, slog.New(slog.DiscardHandler))

	s := &Server{
		cfg: &config.Config{
			Server: config.Server{Listen: "127.0.0.1:0", BaseURL: "http://localhost"},
		},
		store:   store,
		req:     nil, // only the routes that do not start workflows are exercised
		tmdb:    nil,
		tmpl:    mustTemplates(t),
		log:     slog.New(slog.DiscardHandler),
		version: "test",
	}
	return s, store
}

func dropDatabase(adminDSN, name string) {
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return
	}
	defer conn.Close(ctx)

	_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// client is a browser: it keeps cookies and follows redirects.
type client struct {
	t    *testing.T
	srv  *httptest.Server
	http *http.Client
}

func newClient(t *testing.T, s *Server) *client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}

	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)

	return &client{
		t:   t,
		srv: srv,
		http: &http.Client{
			Jar: jar,
			// Redirects are followed, which is what a browser does and what
			// makes these tests catch a handler that renders instead of
			// redirecting.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func (c *client) get(path string) *http.Response {
	c.t.Helper()

	resp, err := c.http.Get(c.srv.URL + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (c *client) body(path string) string {
	c.t.Helper()

	resp := c.get(path)
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// post submits a form with a valid token.
//
// The token is read from the cookie rather than scraped out of a page, and
// that is not a shortcut: the double-submit check compares the field against
// the cookie, so the cookie's value is by definition the one that passes. It
// also means the helper cannot be defeated by the form being on a different
// path from the action it posts to — which is the case for every action here.
func (c *client) post(path string, form url.Values) *http.Response {
	c.t.Helper()

	form.Set(csrfField, c.csrfToken())

	resp, err := c.http.PostForm(c.srv.URL+path, form)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (c *client) csrfToken() string {
	c.t.Helper()

	// Any page issues one, so asking for the login form is enough even for a
	// browser that has not signed in.
	c.get("/login").Body.Close()

	u, err := url.Parse(c.srv.URL)
	if err != nil {
		c.t.Fatalf("parse the server url: %v", err)
	}

	for _, cookie := range c.http.Jar.Cookies(u) {
		if cookie.Name == csrfCookie {
			return cookie.Value
		}
	}
	c.t.Fatal("the server never issued a csrf cookie")
	return ""
}

// signedIn completes the setup wizard and returns a browser holding a session.
func signedIn(t *testing.T, s *Server) *client {
	t.Helper()

	c := newClient(t, s)
	resp := c.post("/setup", url.Values{
		"username": {"alex"},
		"password": {"correct-horse-battery"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup returned %d", resp.StatusCode)
	}
	return c
}

// ---------------------------------------------------------------------------
// First run
// ---------------------------------------------------------------------------

func TestFreshInstanceRedirectsToSetup(t *testing.T) {
	s, _ := newTestServer(t)
	c := newClient(t, s)

	for _, path := range []string{"/", "/search", "/requests", "/settings"} {
		resp := c.get(path)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, resp.StatusCode)
		}
		// The final URL after redirects is what matters: a fresh instance must
		// land on the wizard rather than on a login form for an account that
		// cannot exist yet.
		if got := resp.Request.URL.Path; got != "/setup" {
			t.Errorf("GET %s landed on %s, want /setup", path, got)
		}
	}
}

func TestSetupCreatesAnAdminAndSignsIn(t *testing.T) {
	s, store := newTestServer(t)

	c := signedIn(t, s)

	// Signed in, so the dashboard renders rather than redirecting.
	if body := c.body("/"); !strings.Contains(body, "Что качается") {
		t.Errorf("the dashboard did not render: %s", truncateForTest(body, 200))
	}

	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || !users[0].IsAdmin {
		t.Fatalf("want one administrator, got %+v", users)
	}
}

// TestSetupClosesBehindItself — leaving the wizard reachable would let anyone
// who can reach the host create an administrator.
func TestSetupClosesBehindItself(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)

	before, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}

	// A second submission, from a browser with no session.
	stranger := newClient(t, s)
	resp := stranger.post("/setup", url.Values{
		"username": {"intruder"},
		"password": {"another-long-secret"},
	})
	resp.Body.Close()

	after, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the wizard created a second account: %d users, want %d", len(after), len(before))
	}
	_ = c
}

func TestSetupRejectsAShortPassword(t *testing.T) {
	s, store := newTestServer(t)
	c := newClient(t, s)

	resp := c.post("/setup", url.Values{"username": {"alex"}, "password": {"short"}})
	body := readBody(t, resp)

	if !strings.Contains(body, "Пароль — минимум") {
		t.Errorf("the page does not explain the rule: %s", truncateForTest(body, 300))
	}

	has, err := store.HasAccounts(context.Background())
	if err != nil {
		t.Fatalf("HasAccounts: %v", err)
	}
	if has {
		t.Error("an account was created despite the rejected password")
	}
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLoginRejectsAWrongPassword(t *testing.T) {
	s, _ := newTestServer(t)
	signedIn(t, s) // creates the account

	c := newClient(t, s)
	resp := c.post("/login", url.Values{
		"username": {"alex"},
		"password": {"not-the-password"},
	})
	body := readBody(t, resp)

	// The message is the same for a wrong password and a missing account, so
	// that the form cannot be used to enumerate names.
	if !strings.Contains(body, "Неверное имя или пароль") {
		t.Errorf("expected a refusal, got: %s", truncateForTest(body, 300))
	}

	// And it did not sign anybody in: the dashboard still redirects.
	if got := c.get("/").Request.URL.Path; got != "/login" {
		t.Errorf("a failed login left the browser at %s", got)
	}
}

func TestLoginWithAMissingAccountLooksTheSame(t *testing.T) {
	s, _ := newTestServer(t)
	signedIn(t, s)

	c := newClient(t, s)
	body := readBody(t, c.post("/login", url.Values{
		"username": {"nobody"},
		"password": {"correct-horse-battery"},
	}))

	if !strings.Contains(body, "Неверное имя или пароль") {
		t.Errorf("a missing account and a wrong password must be indistinguishable: %s",
			truncateForTest(body, 300))
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)

	resp := c.post("/logout", url.Values{})
	resp.Body.Close()

	if got := c.get("/").Request.URL.Path; got != "/login" {
		t.Errorf("after logging out the browser landed on %s", got)
	}

	var count int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("%d sessions survived a logout", count)
	}
}

// TestLoginReturnsToWhereYouWereHeaded — the next parameter is a convenience,
// and the point of the test is that it cannot be turned into an open redirect.
func TestLoginReturnsToWhereYouWereHeaded(t *testing.T) {
	s, _ := newTestServer(t)
	signedIn(t, s)

	c := newClient(t, s)

	// Asking for a protected page sends the browser to the login form.
	resp := c.get("/requests")
	resp.Body.Close()

	if got := resp.Request.URL.Path; got != "/login" {
		t.Fatalf("landed on %s, want the login form", got)
	}

	body := readBody(t, c.post("/login", url.Values{
		"username": {"alex"},
		"password": {"correct-horse-battery"},
		"next":     {"/requests"},
	}))

	if !strings.Contains(body, "Все запросы") {
		t.Errorf("the login did not return to the requested page: %s", truncateForTest(body, 200))
	}
}

func TestSafeNextRefusesAnotherHost(t *testing.T) {
	// A redirect target taken from a parameter is a phishing primitive unless
	// it is forced to be a local path.
	for _, raw := range []string{
		"https://evil.example.com/",
		"//evil.example.com/",
		"",
	} {
		if got := safeNext(raw); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", raw, got)
		}
	}

	if got := safeNext("/requests/7"); got != "/requests/7" {
		t.Errorf("safeNext kept nothing: %q", got)
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

func TestPostWithoutATokenIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	c := signedIn(t, s)

	// A cross-site form cannot read the cookie, so it cannot produce a
	// matching field. This is what stands in for it.
	resp, err := c.http.PostForm(c.srv.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	// And the session survived, which is the point: a rejected request must
	// not have done the thing.
	if got := c.get("/").Request.URL.Path; got != "/" {
		t.Errorf("the rejected request still logged the browser out: %s", got)
	}
}

func TestPostWithAMismatchedTokenIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	c := signedIn(t, s)

	resp, err := c.http.PostForm(c.srv.URL+"/logout", url.Values{csrfField: {"not-the-token"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// seedRequest inserts a request row, since starting one needs TMDB.
//
// Each call gets its own tmdb id, because media is unique on it: a test that
// seeds two requests would otherwise fail on a constraint rather than on the
// thing it was checking.
var seedCounter int64

func seedRequest(t *testing.T, store *storage.Store, state storage.State) int64 {
	t.Helper()

	ctx := context.Background()
	pool := store.Pool()

	seedCounter++
	tmdbID := 600 + seedCounter

	var mediaID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO media (media_type, tmdb_id, title, title_ru, year, poster_path, overview)
		VALUES ('movie', $1, 'The Matrix', 'Матрица', 1999, '/p.jpg', 'Описание.')
		RETURNING id`, tmdbID).Scan(&mediaID)
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}

	var id int64
	err = pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, attempt_count, progress,
		                      dl_speed, eta_seconds, selected_title, selected_seeders,
		                      search_deadline, qbit_hash)
		VALUES ($1, 'movie-603', $2, 12, 0.43, 12582912, 4800, 'Матрица 1080p | Дубляж', 51,
		        now() + interval '700 days', 'abc123')
		RETURNING id`, mediaID, state).Scan(&id)
	if err != nil {
		t.Fatalf("insert request: %v", err)
	}
	return id
}

func TestDashboardShowsARequest(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)
	seedRequest(t, store, storage.StateDownloading)

	body := c.body("/")

	for _, want := range []string{"Матрица (1999)", "downloading", "43.0%", "12.0 МиБ/s"} {
		if !strings.Contains(body, want) {
			t.Errorf("the dashboard does not show %q", want)
		}
	}
}

// TestActiveFragmentReturnsOnlyTheTable — htmx swaps the response into the
// element that asked for it, so a full page would nest a layout inside a
// table cell.
func TestActiveFragmentReturnsOnlyTheTable(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)
	seedRequest(t, store, storage.StateDownloading)

	resp := c.get("/fragments/active")
	defer resp.Body.Close()

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: a cached fragment shows a frozen bar", got)
	}

	body := readBody(t, resp)
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") {
		t.Error("the fragment returned a whole page")
	}
	if !strings.Contains(body, "Матрица (1999)") {
		t.Errorf("the fragment does not show the request: %s", truncateForTest(body, 200))
	}
}

// TestOnlyActiveRequestsAreInTheFragment — the list is polled every three
// seconds, so it must not grow with history.
func TestOnlyActiveRequestsAreInTheFragment(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)

	seedRequest(t, store, storage.StateCompleted)
	seedRequest(t, store, storage.StateFailed)

	body := c.body("/fragments/active")
	if strings.Contains(body, "Матрица") {
		t.Errorf("a finished request appeared in the live fragment: %s", truncateForTest(body, 300))
	}
	if !strings.Contains(body, "Ничего не качается") {
		t.Errorf("the empty state did not render: %s", truncateForTest(body, 300))
	}
}

func TestRequestDetailAndFilters(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)
	id := seedRequest(t, store, storage.StateDownloading)

	t.Run("detail", func(t *testing.T) {
		body := c.body("/requests/" + strconv.FormatInt(id, 10))

		for _, want := range []string{
			"Матрица (1999)", "abc123", "Матрица 1080p | Дубляж", "movie-603",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the detail page does not show %q", want)
			}
		}
	})

	t.Run("missing", func(t *testing.T) {
		resp := c.get("/requests/99999")
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("bad id", func(t *testing.T) {
		resp := c.get("/requests/not-a-number")
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown state filter", func(t *testing.T) {
		resp := c.get("/requests?state=nonsense")
		defer resp.Body.Close()

		// A typo must not read as "there is nothing in flight".
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("by state", func(t *testing.T) {
		body := c.body("/requests?state=failed")
		if strings.Contains(body, "Матрица") {
			t.Errorf("a downloading request appeared under state=failed: %s", truncateForTest(body, 300))
		}
	})
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

func TestNotificationsInbox(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)
	id := seedRequest(t, store, storage.StateCompleted)

	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	userID := users[0].ID

	// Notifications go to a request's watchers, so without this the inbox is
	// legitimately empty and the test would be asserting the wrong thing.
	if err := store.AddWatcher(context.Background(), id, userID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}

	if _, err := store.CreateNotifications(context.Background(), id,
		storage.NotificationCompleted, "Матрица готова", "Файл на месте", nil); err != nil {
		t.Fatalf("create notification: %v", err)
	}

	body := c.body("/notifications")
	if !strings.Contains(body, "Матрица готова") {
		t.Errorf("the inbox does not show the notification: %s", truncateForTest(body, 300))
	}

	// The badge counts it.
	if !strings.Contains(c.body("/"), "badge") {
		t.Error("the unread badge did not appear")
	}

	// Marking it read clears it.
	unread, err := store.ListNotifications(context.Background(), userID, true, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(unread) != 1 {
		t.Fatalf("want one unread notification, got %d", len(unread))
	}

	resp := c.post("/notifications/read", url.Values{
		"id": {strconv.FormatInt(unread[0].ID, 10)},
	})
	resp.Body.Close()

	if n, err := store.UnreadCount(context.Background(), userID); err != nil {
		t.Fatalf("UnreadCount: %v", err)
	} else if n != 0 {
		t.Errorf("%d notifications are still unread", n)
	}
}

// TestNotificationsCannotBeMarkedReadAcrossUsers — a notification id is
// visible in a URL, so the statement has to be scoped to the session's user.
func TestNotificationsCannotBeMarkedReadAcrossUsers(t *testing.T) {
	s, store := newTestServer(t)
	signedIn(t, s)
	id := seedRequest(t, store, storage.StateCompleted)

	ctx := context.Background()

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	owner := &users[0]

	if err := store.AddWatcher(ctx, id, owner.ID); err != nil {
		t.Fatalf("add watcher: %v", err)
	}
	if _, err := store.CreateNotifications(ctx, id,
		storage.NotificationCompleted, "Чужое уведомление", "", nil); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	unread, err := store.ListNotifications(ctx, owner.ID, true, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(unread) != 1 {
		t.Fatalf("want one notification, got %d", len(unread))
	}

	// Another user's session names the same id.
	if _, err := store.CreateAccount(ctx, "bob", "another-long-secret", false); err != nil {
		t.Fatalf("create the second account: %v", err)
	}
	c2 := newClient(t, s)
	c2.post("/login", url.Values{"username": {"bob"}, "password": {"another-long-secret"}}).Body.Close()

	resp := c2.post("/notifications/read", url.Values{
		"id": {strconv.FormatInt(unread[0].ID, 10)},
	})
	resp.Body.Close()

	if n, err := store.UnreadCount(ctx, owner.ID); err != nil {
		t.Fatalf("UnreadCount: %v", err)
	} else if n != 1 {
		t.Errorf("another user cleared somebody else's inbox")
	}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func TestInviteFlow(t *testing.T) {
	s, store := newTestServer(t)
	admin := signedIn(t, s)
	ctx := context.Background()

	resp := admin.post("/settings/invites", url.Values{})
	resp.Body.Close()

	invites, err := store.ListInvites(ctx, 10)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 1 {
		t.Fatalf("want one invite, got %d", len(invites))
	}
	code := invites[0].Code

	// A stranger registers with it.
	bob := newClient(t, s)
	joinBody := bob.body("/join?code=" + code)
	if !strings.Contains(joinBody, "Регистрация по приглашению") {
		t.Fatalf("the join page did not render: %s", truncateForTest(joinBody, 300))
	}

	resp = bob.post("/join", url.Values{
		"code": {code}, "username": {"bob"}, "password": {"another-long-secret"},
	})
	body := readBody(t, resp)

	if !strings.Contains(body, "Что качается") {
		t.Errorf("registration did not sign the new user in: %s", truncateForTest(body, 300))
	}

	// And it is not an administrator.
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Username == "bob" && u.IsAdmin {
			t.Error("an invited account was created as an administrator")
		}
	}

	// The code is spent.
	if !strings.Contains(bob.body("/join?code="+code), "уже использовано") {
		t.Error("the invite was reusable")
	}

	// A second attempt cannot create another account.
	before := len(users)
	stranger := newClient(t, s)
	stranger.post("/join", url.Values{
		"code": {code}, "username": {"carol"}, "password": {"yet-another-secret"},
	}).Body.Close()

	after, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(after) != before {
		t.Errorf("a spent invite created an account: %d users, want %d", len(after), before)
	}
}

func TestNonAdminCannotCreateInvites(t *testing.T) {
	s, store := newTestServer(t)
	ctx := context.Background()

	if _, err := store.CreateAccount(ctx, "bob", "another-long-secret", false); err != nil {
		t.Fatalf("create account: %v", err)
	}

	bob := newClient(t, s)
	bob.post("/login", url.Values{"username": {"bob"}, "password": {"another-long-secret"}}).Body.Close()

	resp := bob.post("/settings/invites", url.Values{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	invites, err := store.ListInvites(ctx, 10)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 0 {
		t.Errorf("a non-admin created %d invites", len(invites))
	}
}

func TestPasswordChangeEndsEverySession(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)
	ctx := context.Background()

	// A second browser signed in as the same person, so that "every session"
	// means more than "this one".
	second := newClient(t, s)
	second.post("/login", url.Values{
		"username": {"alex"}, "password": {"correct-horse-battery"},
	}).Body.Close()

	resp := c.post("/settings/password", url.Values{
		"current": {"correct-horse-battery"}, "password": {"a-brand-new-secret"},
	})
	resp.Body.Close()

	for name, browser := range map[string]*client{"first": c, "second": second} {
		if got := browser.get("/").Request.URL.Path; got != "/login" {
			t.Errorf("the %s browser is still signed in: landed on %s", name, got)
		}
	}

	// The old password no longer works and the new one does.
	if _, err := store.Authenticate(ctx, "alex", "correct-horse-battery"); err == nil {
		t.Error("the old password still authenticates")
	}
	if _, err := store.Authenticate(ctx, "alex", "a-brand-new-secret"); err != nil {
		t.Errorf("the new password does not authenticate: %v", err)
	}
}

func TestPasswordChangeRequiresTheCurrentOne(t *testing.T) {
	s, store := newTestServer(t)
	c := signedIn(t, s)

	body := readBody(t, c.post("/settings/password", url.Values{
		"current": {"not-it"}, "password": {"a-brand-new-secret"},
	}))

	if !strings.Contains(body, "Текущий пароль неверен") {
		t.Errorf("the change was not refused with an explanation: %s", truncateForTest(body, 300))
	}
	if _, err := store.Authenticate(context.Background(), "alex", "a-brand-new-secret"); err == nil {
		t.Error("the password was changed despite the wrong current one")
	}
}

// ---------------------------------------------------------------------------
// Static assets
// ---------------------------------------------------------------------------

func TestStaticAssetsAreServed(t *testing.T) {
	s, _ := newTestServer(t)
	c := newClient(t, s)

	for _, path := range []string{"/static/style.css", "/static/htmx.min.js"} {
		resp := c.get(path)
		body := readBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned nothing", path)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return string(b)
}
