package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/tmdb"
)

// The poster endpoint exists because the browser may not be able to reach
// image.tmdb.org at all. What these tests pin is that the browser never has to:
// every URL the pages build points at us, and what we serve is fetched through
// the same client — and therefore the same proxy — as the API calls.

// ---------------------------------------------------------------------------
// Building the URL
// ---------------------------------------------------------------------------

// TestPosterPathPointsAtUs is the invariant the whole change rests on.
//
// A single tmdb.PosterURL left in a view puts image.tmdb.org back into the
// page, and the failure is invisible from the server's side: the page renders,
// nothing is logged, and the only symptom is broken images in a browser that
// cannot reach TMDB.
func TestPosterPathPointsAtUs(t *testing.T) {
	got := posterPath("/abc123.jpg", "w342")

	if !strings.HasPrefix(got, "/poster/") {
		t.Fatalf("posterPath = %q, want a path on this server", got)
	}
	if strings.Contains(got, "image.tmdb.org") {
		t.Fatalf("posterPath = %q, which points the browser at TMDB", got)
	}
	if !strings.HasSuffix(got, "/abc123.jpg") {
		t.Errorf("posterPath lost the image path: %q", got)
	}
}

func TestPosterPathSizes(t *testing.T) {
	if got := posterPath("/x.jpg", ""); !strings.Contains(got, defaultPosterSize) {
		t.Errorf("an empty size should fall back to the default: %q", got)
	}
	if got := posterPath("/x.jpg", "original"); !strings.Contains(got, "original") {
		t.Errorf("posterPath = %q", got)
	}
}

// TestPosterPathEmpty — the templates guard on an empty URL to render a
// placeholder, so an absent poster must produce an empty string rather than a
// URL that 404s.
func TestPosterPathEmpty(t *testing.T) {
	if got := posterPath("", "w342"); got != "" {
		t.Errorf("posterPath(\"\") = %q, want empty", got)
	}
}

// TestPosterPathIsARoutableURL — the route is /poster/{size}/{path...}, so a
// URL with an empty segment or a trailing slash would not match and every
// poster would 404.
func TestPosterPathIsARoutableURL(t *testing.T) {
	got := posterPath("/a b.jpg", "w342")

	if strings.Contains(got, " ") {
		t.Errorf("posterPath left a space unescaped: %q", got)
	}
	if strings.Contains(got, "//") {
		t.Errorf("posterPath = %q, which contains an empty segment", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Errorf("posterPath produced an unparseable URL %q: %v", got, err)
	}

	parts := strings.Split(strings.TrimPrefix(got, "/poster/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		t.Errorf("posterPath = %q, which the route pattern would not match", got)
	}
}

// ---------------------------------------------------------------------------
// What the pages emit
// ---------------------------------------------------------------------------

// TestPagesNeverLinkToTMDB walks the rendered pages looking for a direct TMDB
// URL.
//
// Checking the rendered HTML rather than the helper is deliberate: the helper
// can be right while a template hardcodes a URL, and that is the form the
// mistake actually takes.
func TestPagesNeverLinkToTMDB(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	for name, data := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data)

			body := rec.Body.String()
			for _, host := range []string{"image.tmdb.org", "api.themoviedb.org"} {
				if strings.Contains(body, host) {
					t.Errorf("%s links directly to %s; a browser behind a proxy cannot load it",
						name, host)
				}
			}
		})
	}
}

// TestSearchCardsUseOurEndpoint — the search grid is where posters appear, so
// it is worth asserting the URL rather than only the absence of TMDB's.
func TestSearchCardsUseOurEndpoint(t *testing.T) {
	result := tmdb.SearchResult{
		Ref:        domain.MovieRef(603),
		Title:      "Матрица",
		PosterPath: "/poster.jpg",
		Year:       1999,
	}

	card := searchCardFor(result)

	if !strings.HasPrefix(card.PosterURL, "/poster/") {
		t.Fatalf("the search card points at %q", card.PosterURL)
	}
	if card.PosterURL == "" {
		t.Fatal("the card lost its poster")
	}
}

// TestSearchCardWithoutAPoster — poster_path is nullable, and the templates
// rely on an empty URL to draw a placeholder.
func TestSearchCardWithoutAPoster(t *testing.T) {
	result := tmdb.SearchResult{Ref: domain.MovieRef(603), Title: "Матрица", Year: 1999}

	if got := searchCardFor(result).PosterURL; got != "" {
		t.Errorf("PosterURL = %q for a title with no poster, want empty", got)
	}
}

// TestConfirmationUsesOurEndpoint — the other place a poster is shown.
func TestConfirmationUsesOurEndpoint(t *testing.T) {
	spec := domain.MediaSpec{
		Ref:        domain.MovieRef(603),
		Title:      "Матрица",
		PosterPath: "/poster.jpg",
	}

	if got := confirmationFor(spec, "/poster.jpg").PosterURL; !strings.HasPrefix(got, "/poster/") {
		t.Errorf("the confirmation page points at %q", got)
	}
}

// ---------------------------------------------------------------------------
// The route
// ---------------------------------------------------------------------------

// TestPosterRouteMatchesWhatWeBuild checks the route pattern against a URL the
// helper produces.
//
// A path parameter mismatch is the failure mode: /poster/{size}/{path} does
// not match "/poster/w342/ab/cd.jpg", and chi would answer 404 for a URL this
// very package builds.
//
// It goes through the whole router, session middleware included, which is why
// it needs a database: everything under /poster is behind requireAuth, and a
// router built without a store panics the moment the middleware asks it
// whether the instance has accounts. A smaller router carrying only this one
// route would test the pattern against itself rather than against the router
// that actually serves it.
func TestPosterRouteMatchesWhatWeBuild(t *testing.T) {
	s, _ := newTestServer(t)

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Redirects are not followed: the answer to look at is the middleware's,
	// and following it would land on the setup page and hide a 404.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, tc := range []struct{ name, path string }{
		{"single segment", posterPath("/abc123.jpg", "w342")},
		{"nested segments", posterPath("/ab/cd123.jpg", "w500")},
		{"original", posterPath("/abc123.jpg", "original")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := srv.URL + tc.path

			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			defer resp.Body.Close()

			// Unauthenticated, so the middleware sends the browser to the setup
			// wizard. What matters is that it is not a 404: the route matched,
			// and the handler is reachable once there is a session.
			if resp.StatusCode == http.StatusNotFound {
				t.Errorf("the poster route did not match %q", url)
			}
			if resp.StatusCode >= 500 {
				t.Errorf("the poster route answered %d for %q", resp.StatusCode, url)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation, which is what keeps this from being an open proxy
// ---------------------------------------------------------------------------

// TestFetchPosterRejectsBadInput — both values are concatenated into an
// upstream URL, so a handler that passed them through would let a request
// reach any TMDB endpoint with our API key on it.
func TestFetchPosterRejectsBadInput(t *testing.T) {
	client := testTMDBClient(t)

	for _, tc := range []struct {
		name string
		path string
		size string
	}{
		{"traversal", "/../../search/movie", "w342"},
		{"absolute url", "https://evil.example.com/x.jpg", "w342"},
		{"query string", "/x.jpg?api_key=leak", "w342"},
		{"fragment", "/x.jpg#x", "w342"},
		{"double slash", "//evil.example.com/x.jpg", "w342"},
		{"unknown size", "/x.jpg", "w9999"},
		{"empty size-like path", "", "w342"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poster, err := client.FetchPoster(t.Context(), tc.path, tc.size)
			if err == nil {
				poster.Close()
				t.Fatalf("FetchPoster(%q, %q) was accepted", tc.path, tc.size)
			}
		})
	}
}

// TestAllowedPosterSizesIsClosed — the set is a whitelist, so a value that is
// not in it must not reach the URL.
func TestAllowedPosterSizesIsClosed(t *testing.T) {
	for _, size := range []string{"w342", "w500", "original"} {
		if !allowedForTest(size) {
			t.Errorf("%q should be allowed", size)
		}
	}
	for _, size := range []string{"", "w9999", "../../etc", "w342/../../x"} {
		if allowedForTest(size) {
			t.Errorf("%q should not be allowed", size)
		}
	}
}

// TestPosterHandlerFetchesTheRightImage is the test that would have caught the
// bug this endpoint was written with.
//
// The first version read the path from PathValue("path"), which is always
// empty — chi names its wildcard "*" — so the handler requested the image
// host's root for every poster and failed on all of them. The route test above
// did not notice, because it only asserted "not 404", and a 502 is not a 404.
//
// So this one goes all the way through: signed in, a real request to the
// endpoint, and the upstream URL asserted. It is the only form of the test
// that can tell a working proxy from a handler that never asks for anything.
func TestPosterHandlerFetchesTheRightImage(t *testing.T) {
	s, _ := newTestServer(t)

	var (
		mu       sync.Mutex
		upstream []string
	)

	// A stub image host, so the assertion is about what we asked for rather
	// than about TMDB's answer.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstream = append(upstream, r.URL.Path)
		mu.Unlock()

		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer origin.Close()

	// The server's own TMDB client is replaced with one pointed at the stub.
	// Same package, so this is a field assignment rather than a constructor.
	client, err := tmdb.NewForTest(origin.URL)
	if err != nil {
		t.Fatalf("build the stub tmdb client: %v", err)
	}
	s.tmdb = client

	// The client builds its own server around s.routes(), and its jar carries
	// the session cookie, so nothing here has to attach one by hand.
	c := newClient(t, s)
	c.post("/setup", url.Values{
		"username": {"alex"},
		"password": {"correct-horse-battery"},
	}).Body.Close()

	for _, tc := range []struct {
		name      string
		posterURL string
		wantPath  string
	}{
		{"flat path", posterPath("/abc123.jpg", "w342"), "/t/p/w342/abc123.jpg"},
		{"nested path", posterPath("/ab/cd123.jpg", "w500"), "/t/p/w500/ab/cd123.jpg"},
		{"original size", posterPath("/abc123.jpg", "original"), "/t/p/original/abc123.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := c.http.Get(c.srv.URL + tc.posterURL)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.posterURL, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, body)
			}
			if string(body) != "jpeg-bytes" {
				t.Errorf("body = %q, want the image through from the origin", body)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
				t.Errorf("content type = %q", ct)
			}
			if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
				t.Errorf("Cache-Control = %q, want it cacheable", cc)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()

	want := []string{
		"/t/p/w342/abc123.jpg",
		"/t/p/w500/ab/cd123.jpg",
		"/t/p/original/abc123.jpg",
	}
	if len(upstream) != len(want) {
		t.Fatalf("the origin saw %d requests, want %d: %v", len(upstream), len(want), upstream)
	}
	for i, w := range want {
		if upstream[i] != w {
			t.Errorf("upstream request %d = %q, want %q", i, upstream[i], w)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func allowedForTest(size string) bool { return tmdb.AllowedPosterSizes[size] }

// testTMDBClient points a TMDB client at a stub host.
//
// It serves nothing: the validation tests never reach the network. Making a
// real request fail loudly is the point — a test that quietly called TMDB
// would be slow, flaky and dependent on an API key.
func testTMDBClient(t *testing.T) *tmdb.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the test made a real request to %s", r.URL.Path)
		http.Error(w, "not stubbed", http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)

	client, err := tmdb.NewForTest(srv.URL)
	if err != nil {
		t.Fatalf("build the tmdb client: %v", err)
	}
	return client
}
