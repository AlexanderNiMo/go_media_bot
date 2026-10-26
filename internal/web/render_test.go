package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
)

// The template tests render every page and every fragment.
//
// They exist because the mistake they catch is invisible until a page is
// opened, and there are eleven of them. Two variants of it were found by hand
// while building this stage: a form missing the CSRF field, and a token
// referenced as .CSRF from inside a range, where "." is the row rather than
// the page. Both produce a blank 500 on one page and nothing anywhere else.
//
// Nothing here needs a database: the templates take view models, and the view
// models are built from structs.

// sampleRequest is a request with every field populated.
//
// Every field, deliberately: a template that reaches for something the
// fixtures leave zero is a template that fails on the one request that has it.
func sampleRequest() *storage.Request {
	now := time.Now()
	season := 2

	return &storage.Request{
		ID:              7,
		MediaID:         3,
		WorkflowID:      "tv-1399-s2",
		FirstRunID:      "run-1",
		State:           storage.StateDownloading,
		AttemptCount:    12,
		ErrorStreak:     1,
		StallCount:      2,
		LastError:       "три релиза подряд не докачались",
		NextRunAt:       &now,
		SearchDeadline:  now.Add(700 * 24 * time.Hour),
		SelectedTitle:   "Игра престолов (2011) 1080p BDRemux | Дубляж",
		SelectedIndexer: "RuTracker",
		SelectedSize:    21 << 30,
		SelectedSeeders: 51,
		SelectedScore:   0.87,
		QbitHash:        "abc123def456",
		ContentPath:     "/downloads/Game of Thrones/S02",
		Progress:        0.4321,
		DlSpeed:         12 << 20,
		ETASeconds:      4800,
		CreatedAt:       now.Add(-3 * time.Hour),
		UpdatedAt:       now,
		CompletedAt:     &now,
		Media: domain.MediaSpec{
			Ref:        domain.SeasonRef(1399, season),
			Title:      "Game of Thrones",
			TitleRU:    "Игра престолов",
			Year:       2011,
			RuntimeMin: 57,
			PosterPath: "/poster.jpg",
			Overview:   "Описание сериала.",
		},
		InPlex:    true,
		PlexTitle: "Game of Thrones",
		PlexYear:  2011,
	}
}

// testPage is the layout data every fixture starts from.
func testPage() page {
	return page{
		User:      &storage.User{ID: 1, Username: "alex", IsAdmin: true},
		Nav:       "dashboard",
		Unread:    3,
		Flash:     "Запрос на «Матрица» создан.",
		FlashKind: "ok",
		CSRF:      "token-abcdefghijklmnop",
		Version:   "test",
	}
}

// fixtures is one fully populated data set per page.
//
// Fully populated on purpose: a template that reaches for a field the fixture
// leaves empty is a template that breaks on the one request that has it, and
// the request somebody opens a page to look at is exactly that one.
func fixtures(t *testing.T) map[string]any {
	t.Helper()

	now := time.Now()
	req := sampleRequest()
	row := rowFor(req, []storage.Watcher{{Username: "alex"}}, now)
	detail := detailFor(req, []storage.Watcher{{Username: "alex"}},
		[]storage.EventRow{
			{
				At:        now,
				Event:     "search.selected",
				FromState: storage.StateSearching,
				ToState:   storage.StateAdding,
				Actor:     "workflow",
				Detail:    []byte(`{"score":0.87,"title":"Матрица"}`),
			},
			{At: now, Event: "no detail", Actor: "workflow"},
			{At: now, Event: "broken detail", Detail: []byte(`not json`)},
		}, now)

	return map[string]any{
		"dashboard": dashboardView{
			page: testPage(),
			// A filter with something in every field: the dashboard's two
			// forms mirror each other's controls, and a mirror only shows
			// up when the value is not empty.
			Filter: dashboardFilter{
				Q:          "матрица",
				Type:       "movie",
				Sort:       "date",
				Since:      "7d",
				ActiveSubs: []string{"downloading", "adding"},
			},
			Active:    []requestRow{row},
			Recent:    []requestRow{row},
			ActiveAll: storage.ActiveStates(),
		},
		"search": searchView{
			page:  testPage(),
			Query: "матрица",
			Results: []searchCard{{
				Type: "movie", TMDBID: 603, Title: "Матрица", Original: "The Matrix",
				// A path on this server, because that is what the pages emit:
				// a fixture with the direct TMDB URL would make the page look
				// correct while teaching nothing about what production does.
				Year: 1999, Kind: "фильм", PosterURL: "/poster/w342/p.jpg",
			}},
		},
		"seasons": seasonsView{
			page:    testPage(),
			Show:    showHeader{TMDBID: 1399, Title: "Игра престолов", Year: 2011},
			Seasons: []seasonCard{{Number: 2, Episodes: 10, AirDate: "2012-04-01"}, {Number: 8, Upcoming: true}},
		},
		"confirm": confirmView{
			page: testPage(),
			confirmation: confirmation{
				MediaType: "tv", TMDBID: 1399, Title: "Игра престолов", Kind: "Сериал",
				Year: 2011, Runtime: 57, Overview: "Описание.", HasSeason: true, Season: 2,
			},
		},
		"requests": requestsView{
			page:     testPage(),
			State:    "all",
			States:   []string{"pending", "failed"},
			Requests: []requestRow{row},
		},
		"request": requestView{page: testPage(), Request: detail},
		"notifications": notificationsView{
			page: testPage(),
			Items: []notificationRow{{
				ID: 1, RequestID: 7, Title: "Матрица", Body: "Готово.", When: "2026-01-01 00:00", Unread: true,
			}},
		},
		"settings": settingsView{
			page:      testPage(),
			Invites:   []inviteRow{{Code: "abc", CreatedAt: "2026-01-01", ExpiresAt: "2026-01-08", State: "действует"}},
			Users:     []storage.User{{ID: 1, Username: "alex", IsAdmin: true}, {ID: 2}},
			InviteTTL: "7 дн",
			// BotUsername is empty in the smoke fixture; a separate test
			// exercises the populated case.
			BotUsername: "mediad_bot",
		},
		"login": loginView{page: testPage(), Next: "/requests"},
		"help":  helpView{page: testPage()},
		"setup": struct{ page }{testPage()},
		"join": struct {
			page
			Code        string
			InviteError string
		}{testPage(), "invite-code", ""},
	}
}

// TestEveryPageRenders is the guard.
func TestEveryPageRenders(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	cases := fixtures(t)

	// A page added without a fixture fails here rather than going unrendered.
	for _, name := range pageNames {
		if _, ok := cases[name]; !ok {
			t.Errorf("template %q has no fixture, so nothing checks that it renders", name)
		}
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/", nil)

			s.render(rec, r, name, data)

			if rec.Code != http.StatusOK {
				t.Fatalf("rendering %s returned %d: %s", name, rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "<!DOCTYPE html>") {
				t.Errorf("rendering %s did not produce a page: %s", name, truncateForTest(body, 200))
			}
		})
	}
}

// TestEveryPageRendersWithAPopulatedRequest does the same with data in every
// field.
//
// The empty case above catches a template that cannot render at all; this one
// catches a template that breaks on a request that has a release, a torrent
// hash, an error and a completion time — which is exactly the request somebody
// opens the page to look at.
func TestRequestPagesRenderPopulated(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	req := sampleRequest()
	now := time.Now()

	detail := detailFor(req, []storage.Watcher{{Username: "alex"}},
		[]storage.EventRow{
			{
				At:        now,
				Event:     "search.selected",
				FromState: storage.StateSearching,
				ToState:   storage.StateAdding,
				Actor:     "workflow",
				Detail:    []byte(`{"score":0.87,"title":"Матрица"}`),
			},
			{At: now, Event: "no detail", Actor: "workflow"},
		}, now)

	p := page{User: &storage.User{ID: 1, Username: "alex"}, CSRF: "token"}

	for _, tc := range []struct {
		name string
		data any
	}{
		{"request", struct {
			page
			Request    requestDetail
			Candidates []storage.Candidate
		}{p, detail, nil}},
		{"requests", struct {
			page
			State    string
			States   []string
			Requests []requestRow
		}{p, "all", []string{"pending", "failed"}, []requestRow{rowFor(req, nil, now)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), tc.name, tc.data)

			if rec.Code != http.StatusOK {
				t.Fatalf("returned %d: %s", rec.Code, truncateForTest(rec.Body.String(), 300))
			}
		})
	}
}

// TestActiveFragmentRendersPopulated — the fragment is rendered on its own
// path, where the only thing in scope is what the fragment handler passes.
func TestActiveFragmentRendersPopulated(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	now := time.Now()
	data := struct {
		page
		Active []requestRow
	}{
		page:   page{CSRF: "token"},
		Active: []requestRow{rowFor(sampleRequest(), nil, now)},
	}

	rec := httptest.NewRecorder()
	s.renderFragment(rec, httptest.NewRequest(http.MethodGet, "/fragments/active", nil),
		"active_table", data)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d: %s", rec.Code, truncateForTest(rec.Body.String(), 300))
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE") {
		t.Error("the fragment rendered a whole page; htmx would swap the layout into the table")
	}

	// The fragment contains a form, so the token has to reach it. A fragment
	// without one submits a form that the server rejects, which reads as the
	// cancel button being broken.
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Error("the fragment's form has no csrf token")
	}
	if strings.Contains(body, `value=""`) {
		t.Error("the fragment's csrf field is empty")
	}
}

// TestEveryPostFormHasAToken walks the rendered pages looking for a form that
// would be rejected.
//
// This is the invariant the CSRF design rests on, and it is the kind of thing
// that is forgotten the moment somebody adds a page. It is checked by reading
// the HTML rather than by trusting the templates.
func TestEveryPostFormHasAToken(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	// Every page, not a sample: the point is to catch the form somebody adds
	// to a page that already rendered fine.
	for name, data := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data)

			body := rec.Body.String()
			forms := strings.Count(body, `method="post"`)
			tokens := strings.Count(body, `name="csrf_token"`)

			if forms != tokens {
				t.Errorf("%d POST forms but %d csrf fields: a form without one is "+
					"rejected at submission, which looks like a broken button", forms, tokens)
			}
			if strings.Contains(body, `name="csrf_token" value=""`) {
				t.Error("a csrf field rendered empty")
			}
		})
	}
}

// TestNoPageNestsAForm reads the rendered pages and fails on a form inside a
// form.
//
// HTML has no nested forms. The parser drops the inner <form> element, and
// its controls — a submit button above all — become part of the outer form.
// The dashboard had one filter form around the whole page, and the active
// table's cancel form was swallowed by it: the button submitted the filter
// form, the page reloaded, and the request was never cancelled.
//
// The rendered HTML looks right, so the check is on the source: the parser
// is what hides the problem.
func TestNoPageNestsAForm(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	for name, data := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data)

			depth := 0
			z := html.NewTokenizer(strings.NewReader(rec.Body.String()))
			for {
				switch z.Next() {
				case html.ErrorToken:
					if depth != 0 {
						t.Errorf("%d form(s) were never closed", depth)
					}
					return
				case html.StartTagToken:
					if tag, _ := z.TagName(); string(tag) == "form" {
						depth++
						if depth > 1 {
							t.Errorf("a form inside another form: the parser drops the inner one, " +
								"and its button submits the outer one instead")
						}
					}
				case html.EndTagToken:
					if tag, _ := z.TagName(); string(tag) == "form" {
						depth--
					}
				}
			}
		})
	}
}

// TestCancelButtonsPostToTheCancelRoute checks the button that cancels a
// request, on both pages that show one.
//
// The button's owner is the form the browser submits, and it is not always
// the form the template looks like it is in — a form nested in another one
// does not survive the parser. A cancel button owned by a GET filter form
// reloads the page and cancels nothing, which is exactly the bug this file
// exists to catch.
func TestCancelButtonsPostToTheCancelRoute(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	for _, name := range []string{"dashboard", "request"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, fixtures(t)[name])

			doc, err := html.Parse(strings.NewReader(rec.Body.String()))
			if err != nil {
				t.Fatalf("parse the rendered page: %v", err)
			}

			found := 0
			var walk func(*html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "button" && isCancelButton(n) {
					found++

					owner := owningForm(n)
					if owner == nil {
						t.Error("the cancel button is not inside a form at all")
						return
					}
					method, action := attrForTest(owner, "method"), attrForTest(owner, "action")
					if !strings.EqualFold(method, "post") {
						t.Errorf("the cancel button submits a %q form; it has to be POST", method)
					}
					if !strings.HasSuffix(action, "/cancel") {
						t.Errorf("the cancel button posts to %q; it has to be the cancel route", action)
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(doc)

			if found == 0 {
				t.Error("the page has no cancel button")
			}
		})
	}
}

// TestDashboardFilterFormsCarryEveryControl checks that both of the
// dashboard's filter forms carry every filter control.
//
// The dashboard has two GET forms — the state pills in one, the shared
// filters in the other — because a single form around both would swallow the
// table's cancel form. Each form mirrors the other's controls as hidden
// inputs, so whichever one the user submits, the whole query string travels.
//
// Without the mirrors a pill click would drop the search text, and a change
// of period would drop the state selection.
func TestDashboardFilterFormsCarryEveryControl(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), "dashboard", fixtures(t)["dashboard"])

	doc, err := html.Parse(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("parse the rendered page: %v", err)
	}

	want := map[string]bool{"q": true, "type": true, "sort": true, "since": true, "state": true}

	forms := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" &&
			strings.EqualFold(attrForTest(n, "method"), "get") {
			forms++

			got := map[string]bool{}
			var controls func(*html.Node)
			controls = func(n *html.Node) {
				if n.Type == html.ElementNode {
					switch n.Data {
					case "input", "select", "textarea":
						if name := attrForTest(n, "name"); name != "" {
							got[name] = true
						}
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					controls(c)
				}
			}
			controls(n)

			for name := range want {
				if !got[name] {
					t.Errorf("a filter form does not carry %q; a submission through it "+
						"would drop that part of the query string", name)
				}
			}
			for name := range got {
				if !want[name] {
					t.Errorf("a filter form carries an unknown control %q", name)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if forms != 2 {
		t.Errorf("the dashboard has %d GET forms; it is meant to have two, one per filter row", forms)
	}
}

// TestTemplateSetsDoNotShareContent — each page is parsed into its own set,
// because every one of them defines "content" and a single set would render
// the last-parsed page for all of them.
func TestTemplateSetsDoNotShareContent(t *testing.T) {
	tmpl := mustTemplates(t)

	for _, name := range pageNames {
		if _, ok := tmpl.set[name]; !ok {
			t.Errorf("no template set for %q", name)
		}
	}

	// Two different pages must not produce the same body. Running this on
	// fixtures rather than on one shared value is what makes it meaningful:
	// with identical input, a set that ignored its page and rendered the same
	// one every time would still look correct if the two happened to agree.
	cases := fixtures(t)
	s := &Server{tmpl: tmpl, log: discardLogger()}

	bodies := make(map[string]string, len(pageNames))
	for _, name := range pageNames {
		rec := httptest.NewRecorder()
		s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, cases[name])
		bodies[name] = rec.Body.String()
	}

	seen := make(map[string]string, len(bodies))
	for name, body := range bodies {
		if other, dup := seen[body]; dup {
			t.Errorf("%s and %s render identical bodies; the template sets are sharing content", name, other)
		}
		seen[body] = name
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustTemplates(t *testing.T) *templates {
	t.Helper()

	tmpl, err := newTemplates()
	if err != nil {
		t.Fatalf("parse the templates: %v", err)
	}
	return tmpl
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// attrForTest reads an attribute of a parsed node.
func attrForTest(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// isCancelButton reports whether a button is the one that cancels a request.
//
// The text is matched loosely on purpose: the dashboard says "отменить" and
// the request page says "Отменить", and both are the same button.
func isCancelButton(n *html.Node) bool {
	return strings.Contains(strings.ToLower(textOf(n)), "отмен")
}

// textOf collects the text of a node.
//
// It walks rather than reads the first child, because a button with an icon
// and no text has no first child at all — and a nil dereference on this
// machine hangs the test instead of failing it.
func textOf(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// owningForm returns the form a control belongs to: the nearest ancestor
// form, which is the one the browser submits it with.
func owningForm(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "form" {
			return p
		}
	}
	return nil
}

// discardLogger is a logger that writes nowhere, for tests that only care
// whether rendering succeeded.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestPosterURLAbsorbsBasePath — a poster is a URL the browser fetches
// against the same origin, so the rendered <img src> has to carry the
// configured base path. Without this, a deployment behind a reverse proxy
// at /mediad renders <img src="/poster/w342/x.jpg"> and every poster
// 404s in the browser; the server has no idea, because the failure is
// on the static-asset path, not on a request the server sees.
func TestPosterURLAbsorbsBasePath(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	data := searchView{
		page:  page{BasePath: "/mediad", CSRF: "token"},
		Query: "матрица",
		Results: []searchCard{{
			Type: "movie", TMDBID: 603, Title: "Матрица", Original: "The Matrix",
			Year: 1999, Kind: "фильм", PosterURL: "/poster/w342/p.jpg",
		}},
	}

	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/mediad/search", nil), "search", data)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d: %s", rec.Code, truncateForTest(rec.Body.String(), 300))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `src="/mediad/poster/w342/p.jpg"`) {
		t.Errorf("poster URL did not absorb the base path; rendered body:\n%s",
			truncateForTest(body, 600))
	}
	// The card link also has to carry the base path. A wrong href here means
	// the user clicks and lands on a 404 from the upstream reverse proxy,
	// not from this server.
	if !strings.Contains(body, `href="/mediad/add/movie/603"`) {
		t.Errorf("search card link did not absorb the base path; rendered body:\n%s",
			truncateForTest(body, 600))
	}
}
