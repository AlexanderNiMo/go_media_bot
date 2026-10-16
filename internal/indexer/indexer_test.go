package indexer

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

func newTestTorznab(t *testing.T, handler http.HandlerFunc) *Torznab {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Indexer.Mode = "torznab"
	cfg.Indexer.BaseURL = srv.URL
	cfg.Indexer.APIKey = "test-key"
	cfg.Indexer.RatePerSecond = 1000 // do not slow the tests down
	cfg.Indexer.CapsTTL = time.Hour

	return NewTorznab(cfg, slog.New(slog.DiscardHandler))
}

func newTestProwlarr(t *testing.T, handler http.HandlerFunc) *Prowlarr {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Indexer.Mode = "prowlarr"
	cfg.Indexer.BaseURL = srv.URL
	cfg.Indexer.APIKey = "test-key"
	cfg.Indexer.RatePerSecond = 1000

	return NewProwlarr(cfg, slog.New(slog.DiscardHandler))
}

const searchFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>Матрица / The Matrix (1999) BDRemux 1080p Дубляж</title>
      <guid isPermaLink="false">https://tracker/1</guid>
      <pubDate>Mon, 02 Jan 2023 15:04:05 +0300</pubDate>
      <size>15000000000</size>
      <enclosure url="https://tracker/dl/1.torrent" type="application/x-bittorrent"/>
      <torznab:attr name="seeders" value="42"/>
      <torznab:attr name="peers" value="50"/>
      <torznab:attr name="infohash" value="ABCDEF0123456789ABCDEF0123456789ABCDEF01"/>
      <torznab:attr name="category" value="2000"/>
      <torznab:attr name="category" value="2040"/>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"/>
    </item>
    <item>
      <title>Матрица (1999) WEB-DL 2160p</title>
      <guid isPermaLink="false">https://tracker/2</guid>
      <enclosure url="https://tracker/dl/2.torrent" type="application/x-bittorrent"/>
      <torznab:attr name="seeders" value="3"/>
      <torznab:attr name="size" value="8000000000"/>
    </item>
  </channel>
</rss>`

const capsBody = `<?xml version="1.0" encoding="UTF-8"?>
<caps>
  <searching>
    <search available="yes" supportedParams="q"/>
    <movie-search available="yes" supportedParams="q,imdbid"/>
    <tv-search available="yes" supportedParams="q,season,ep,tvdbid"/>
  </searching>
</caps>`

func TestTorznabSearchParsesAttributes(t *testing.T) {
	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("t") {
		case "caps":
			_, _ = w.Write([]byte(capsBody))
		default:
			if got := r.URL.Query().Get("apikey"); got != "test-key" {
				t.Errorf("apikey = %q", got)
			}
			_, _ = w.Write([]byte(searchFeed))
		}
	})

	releases, err := c.Search(context.Background(), SearchRequest{Query: "Матрица", IMDBID: "tt0133093"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(releases))
	}

	first := releases[0]

	if first.Seeders != 42 {
		t.Errorf("seeders = %d, want 42 — the torznab namespace may not be decoded", first.Seeders)
	}
	// peers is seeders + leechers, so leechers is derived.
	if first.Leechers != 8 {
		t.Errorf("leechers = %d, want 8 (50 peers - 42 seeders)", first.Leechers)
	}
	// The infohash is what the blacklist is keyed on, and indexers report it
	// in whatever case they like.
	if first.InfoHash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("infohash = %q, want it lower-cased", first.InfoHash)
	}
	if first.SizeBytes != 15000000000 {
		t.Errorf("size = %d", first.SizeBytes)
	}
	if len(first.Categories) != 2 || first.Categories[0] != 2000 {
		t.Errorf("categories = %v, want [2000 2040]", first.Categories)
	}
	if first.MagnetURI == "" {
		t.Error("magneturl was not read")
	}
	if first.DownloadURL != "https://tracker/dl/1.torrent" {
		t.Errorf("download url = %q", first.DownloadURL)
	}
	if first.PublishedAt.IsZero() {
		t.Error("pubDate was not parsed")
	}
	if first.Title == "" {
		t.Error("title is empty")
	}

	// A release with no magnet must fall back to the enclosure URL.
	if releases[1].MagnetURI != "" {
		t.Error("second release unexpectedly has a magnet")
	}
	if releases[1].DownloadURL == "" {
		t.Error("second release lost its download url")
	}
}

// TestTorznabSizeFallback — size is a required attribute, but some indexers
// put it only in the attribute and some only in the RSS element.
func TestTorznabSizeFallback(t *testing.T) {
	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(capsBody))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
          <rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
            <channel>
              <item>
                <title>Only attribute</title>
                <torznab:attr name="size" value="123456"/>
              </item>
            </channel>
          </rss>`))
	})

	releases, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if releases[0].SizeBytes != 123456 {
		t.Errorf("size = %d, want it read from the attribute", releases[0].SizeBytes)
	}
}

// TestTorznabErrorElement — a Torznab failure is an HTTP 200 carrying an
// <error> element, so the status code alone cannot tell success from failure.
func TestTorznabErrorElement(t *testing.T) {
	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(capsBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
          <error code="100" description="Incorrect user credentials"/>`))
	})

	_, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("an <error> body must surface as an error")
	}

	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if apiErr.Code != 100 {
		t.Errorf("code = %d, want 100", apiErr.Code)
	}
	if !strings.Contains(apiErr.Error(), "Incorrect user credentials") {
		t.Errorf("message = %q", apiErr.Error())
	}
}

func TestTorznabRateLimitIsRecognised(t *testing.T) {
	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(capsBody))
			return
		}
		// Torznab signals "request limit reached" as code 500.
		_, _ = w.Write([]byte(`<?xml version="1.0"?><error code="500" description="Request limit reached"/>`))
	})

	_, err := c.Search(context.Background(), SearchRequest{Query: "x"})

	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if !apiErr.RateLimited() {
		t.Error("code 500 must be treated as a rate limit, not a generic failure")
	}
}

// TestTorznabUsesIdentifiersOnlyWhenSupported is the difference between a
// search that works and one that silently returns nothing: an indexer that
// does not declare a parameter will not error on it, it will simply match
// nothing.
func TestTorznabUsesIdentifiersOnlyWhenSupported(t *testing.T) {
	var lastQuery map[string][]string

	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("t") == "caps" {
			// Advertise a tv-search that does NOT support tvdbid.
			_, _ = w.Write([]byte(`<?xml version="1.0"?><caps><searching>
              <search available="yes" supportedParams="q"/>
              <movie-search available="yes" supportedParams="q"/>
              <tv-search available="yes" supportedParams="q,season"/>
            </searching></caps>`))
			return
		}
		lastQuery = q
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`))
	})

	season := 2
	_, err := c.Search(context.Background(), SearchRequest{
		Query:  "Игра престолов",
		TVDBID: 121361,
		Season: &season,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got := lastQuery["t"][0]; got != "tvsearch" {
		t.Errorf("t = %q, want tvsearch", got)
	}
	if got := lastQuery["season"][0]; got != "2" {
		t.Errorf("season = %q, want 2", got)
	}
	// tvdbid is not in supportedParams, so sending it would match nothing.
	if _, sent := lastQuery["tvdbid"]; sent {
		t.Error("tvdbid was sent even though the indexer does not support it")
	}
	// The text query is always sent: most trackers match on the release name.
	if got := lastQuery["q"][0]; got != "Игра престолов" {
		t.Errorf("q = %q", got)
	}
}

func TestTorznabFallsBackWhenCapsUnavailable(t *testing.T) {
	c := newTestTorznab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel>
          <item><title>t</title><enclosure url="http://x/1.torrent"/></item>
        </channel></rss>`))
	})

	// Caps are an optimisation, not a prerequisite: a text search still works.
	releases, err := c.Search(context.Background(), SearchRequest{Query: "matrix", IMDBID: "tt0133093"})
	if err != nil {
		t.Fatalf("a failed caps lookup must not fail the search: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(releases))
	}
}

// ---------------------------------------------------------------------------
// Prowlarr
// ---------------------------------------------------------------------------

const prowlarrBody = `[
  {
    "title": "Матрица / The Matrix (1999) BDRemux 1080p",
    "guid": "https://prowlarr/1",
    "size": 15000000000,
    "seeders": 42,
    "leechers": 8,
    "downloadUrl": "https://prowlarr/dl/1",
    "magnetUrl": "magnet:?xt=urn:btih:abc",
    "infoHash": "ABC123",
    "indexer": "RuTracker",
    "indexerId": 5,
    "categories": [
      {"id": 2000, "subCategories": []},
      {"id": 2040, "subCategories": []}
    ],
    "publishDate": "2023-01-02T15:04:05Z"
  },
  {
    "title": "Матрица (1999) WEB-DL",
    "guid": "https://prowlarr/2",
    "size": 8000000000,
    "seeders": null,
    "leechers": null,
    "downloadUrl": "https://prowlarr/dl/2",
    "infoHash": "",
    "indexer": "",
    "indexerId": 7,
    "categories": [
      {"id": 2000, "subCategories": [
        {"id": 2040, "subCategories": []}
      ]}
    ],
    "publishDate": "not a date"
  }
]`

func TestProwlarrSearchParsesReleases(t *testing.T) {
	c := newTestProwlarr(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "test-key" {
			t.Errorf("X-Api-Key = %q", got)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1/search") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(prowlarrBody))
	})

	releases, err := c.Search(context.Background(), SearchRequest{Query: "матрица", IMDBID: "tt0133093"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("got %d releases, want 2", len(releases))
	}

	first := releases[0]
	if first.Seeders != 42 || first.Leechers != 8 {
		t.Errorf("seeders/leechers = %d/%d", first.Seeders, first.Leechers)
	}
	if first.Indexer != "RuTracker" {
		t.Errorf("indexer = %q", first.Indexer)
	}
	if first.InfoHash != "abc123" {
		t.Errorf("infohash = %q, want it lower-cased", first.InfoHash)
	}
	if first.PublishedAt.IsZero() {
		t.Error("publishDate was not parsed")
	}
}

// TestProwlarrNullSeedersIsNotZero — a null must not read as "zero seeders",
// or every release from an indexer that omits the field would fail the
// minimum-seeders filter and that indexer would look permanently empty.
func TestProwlarrNullSeedersIsNotZero(t *testing.T) {
	c := newTestProwlarr(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(prowlarrBody))
	})

	releases, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if releases[1].Seeders != -1 {
		t.Errorf("seeders = %d, want -1 to mark it unknown", releases[1].Seeders)
	}

	// An unattributed release still gets a label, so the selector can score it
	// and the UI can show where it came from.
	if releases[1].Indexer != "indexer-7" {
		t.Errorf("indexer = %q, want a fallback naming the indexer id", releases[1].Indexer)
	}
}

func TestProwlarrPassesIdentifiers(t *testing.T) {
	var lastQuery map[string][]string

	c := newTestProwlarr(t, func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.Query()
		_, _ = w.Write([]byte(`[]`))
	})

	season := 3
	_, err := c.Search(context.Background(), SearchRequest{
		Query: "show", TVDBID: 121361, Season: &season,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Prowlarr maps its `type` onto Torznab's `t` internally.
	if got := lastQuery["type"][0]; got != "tvsearch" {
		t.Errorf("type = %q, want tvsearch", got)
	}
	if got := lastQuery["season"][0]; got != "3" {
		t.Errorf("season = %q", got)
	}
	if got := lastQuery["tvdbId"][0]; got != "121361" {
		t.Errorf("tvdbId = %q", got)
	}
}

func TestProwlarrUnauthorized(t *testing.T) {
	c := newTestProwlarr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := c.Search(context.Background(), SearchRequest{Query: "x"})

	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if !apiErr.Unauthorized() {
		t.Error("a 401 must be recognised as an auth failure")
	}
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

func TestForMediaBuildsRequest(t *testing.T) {
	spec := domain.MediaSpec{
		Ref:           domain.SeasonRef(1399, 2),
		Title:         "Game of Thrones",
		TitleRU:       "Игра престолов",
		OriginalTitle: "Game of Thrones",
		IMDBID:        "tt0944947",
		TVDBID:        121361,
	}

	req := ForMedia(spec, []int{5000}, 100)

	// The Russian title is the one the target trackers will have used.
	if req.Query != "Игра престолов" {
		t.Errorf("query = %q, want the Russian title", req.Query)
	}
	if req.Season == nil || *req.Season != 2 {
		t.Errorf("season = %v, want 2", req.Season)
	}
	if req.TVDBID != 121361 {
		t.Errorf("tvdbid = %d", req.TVDBID)
	}
	if req.Limit != 100 {
		t.Errorf("limit = %d", req.Limit)
	}
}

func TestForMediaWithNoLocalisedTitle(t *testing.T) {
	spec := domain.MediaSpec{
		Ref:           domain.MovieRef(603),
		Title:         "The Matrix",
		OriginalTitle: "The Matrix",
	}

	req := ForMedia(spec, nil, 0)

	if req.Query != "The Matrix" {
		t.Errorf("query = %q", req.Query)
	}
	if req.Season != nil {
		t.Error("a film must not carry a season")
	}
}

// ---------------------------------------------------------------------------
// Factory
// ---------------------------------------------------------------------------

func TestNewSelectsMode(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	cases := []struct {
		name    string
		mode    string
		wantErr bool
		wantTyp func(Indexer) bool
	}{
		{"prowlarr", "prowlarr", false, func(i Indexer) bool { _, ok := i.(*Prowlarr); return ok }},
		{"torznab", "torznab", false, func(i Indexer) bool { _, ok := i.(*Torznab); return ok }},
		{"unknown mode", "nonsense", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Indexer.Mode = tc.mode

			idx, err := New(cfg, log)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected an error for mode %q", tc.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%s): %v", tc.mode, err)
			}
			if !tc.wantTyp(idx) {
				t.Errorf("mode %q: got %T, want the right concrete type", tc.mode, idx)
			}
		})
	}
}

// TestProwlarrFlattensNestedCategories — Prowlarr returns a tree of category
// objects, not a flat list of ids, and the release selector works against
// the flat form. The flattening has to recurse into subCategories and
// preserve order.
func TestProwlarrFlattensNestedCategories(t *testing.T) {
	c := newTestProwlarr(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {
		    "title": "X",
		    "guid": "g",
		    "downloadUrl": "u",
		    "indexerId": 1,
		    "categories": [
		      {"id": 2000, "subCategories": [
		        {"id": 2040, "subCategories": [
		          {"id": 2045, "subCategories": []}
		        ]}
		      ]},
		      {"id": 5000, "subCategories": []}
		    ]
		  }
		]`))
	})

	rels, err := c.Search(context.Background(), SearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []int{2000, 2040, 2045, 5000}
	if len(rels[0].Categories) != len(want) {
		t.Fatalf("categories = %v, want %v", rels[0].Categories, want)
	}
	for i, id := range want {
		if rels[0].Categories[i] != id {
			t.Errorf("categories[%d] = %d, want %d", i, rels[0].Categories[i], id)
		}
	}
}
