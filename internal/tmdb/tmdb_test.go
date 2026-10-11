package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// newTestClient points a client at a stub server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.TMDB.APIKey = "test-key"
	cfg.TMDB.Language = "ru-RU"

	c, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseURL = srv.URL
	return c
}

const searchMultiBody = `{
  "page": 1,
  "results": [
    {
      "media_type": "movie",
      "id": 603,
      "title": "Матрица",
      "original_title": "The Matrix",
      "overview": "Хакер узнаёт правду.",
      "poster_path": "/matrix.jpg",
      "release_date": "1999-03-31",
      "vote_average": 8.2,
      "popularity": 40.5
    },
    {
      "media_type": "tv",
      "id": 1399,
      "name": "Игра престолов",
      "original_name": "Game of Thrones",
      "overview": "Семь королевств.",
      "poster_path": "/got.jpg",
      "first_air_date": "2011-04-17",
      "vote_average": 8.4,
      "popularity": 60.1
    },
    {
      "media_type": "person",
      "id": 6384,
      "name": "Кеану Ривз",
      "popularity": 30.0
    },
    {
      "media_type": "movie",
      "id": 999,
      "title": "Фильм без даты",
      "original_title": "Undated Film",
      "poster_path": null,
      "release_date": ""
    }
  ]
}`

func TestSearchMulti(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/multi" {
			t.Errorf("path = %q, want /search/multi", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchMultiBody))
	})

	results, err := c.SearchMulti(context.Background(), "матрица")
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}

	// The person must be dropped: multi-search returns actors alongside
	// titles, and offering one as something to download is nonsense.
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (the person filtered out)", len(results))
	}

	movie := results[0]
	if movie.Ref.Type != domain.MediaTypeMovie || movie.Ref.TMDBID != 603 {
		t.Errorf("movie ref = %+v, want movie/603", movie.Ref)
	}
	if movie.Title != "Матрица" || movie.OriginalTitle != "The Matrix" {
		t.Errorf("movie titles = %q/%q", movie.Title, movie.OriginalTitle)
	}
	if movie.Year != 1999 {
		t.Errorf("movie year = %d, want 1999", movie.Year)
	}
	if movie.PosterPath != "/matrix.jpg" {
		t.Errorf("poster = %q", movie.PosterPath)
	}

	// A series carries name/first_air_date where a film carries
	// title/release_date; the mapping has to normalise both.
	series := results[1]
	if series.Ref.Type != domain.MediaTypeTV || series.Ref.TMDBID != 1399 {
		t.Errorf("series ref = %+v, want tv/1399", series.Ref)
	}
	if series.Title != "Игра престолов" {
		t.Errorf("series title = %q, want the name field", series.Title)
	}
	if series.Year != 2011 {
		t.Errorf("series year = %d, want 2011", series.Year)
	}

	// A series hit names a show, not a season, so it has no season yet.
	if series.Ref.Season != nil {
		t.Error("a search result must not carry a season")
	}

	// A missing date is normal (announced titles) and must not be an error.
	if results[2].Year != 0 {
		t.Errorf("undated film year = %d, want 0", results[2].Year)
	}

	if !strings.Contains(gotQuery, "language=ru-RU") {
		t.Errorf("query %q does not request the configured language", gotQuery)
	}
	if !strings.Contains(gotQuery, "include_adult=false") {
		t.Errorf("query %q does not exclude adult results", gotQuery)
	}
}

func TestSearchMultiRejectsEmptyQuery(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})

	if _, err := c.SearchMulti(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty query")
	}
}

const movieBody = `{
  "id": 603,
  "title": "Матрица",
  "original_title": "The Matrix",
  "overview": "Хакер узнаёт правду.",
  "poster_path": "/matrix.jpg",
  "release_date": "1999-03-31",
  "runtime": 136,
  "imdb_id": "tt0133093"
}`

func TestMovie(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/603" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(movieBody))
	})

	movie, err := c.Movie(context.Background(), 603)
	if err != nil {
		t.Fatalf("Movie: %v", err)
	}

	if movie.Ref != domain.MovieRef(603) {
		t.Errorf("ref = %+v", movie.Ref)
	}
	if movie.RuntimeMin != 136 {
		t.Errorf("runtime = %d, want 136", movie.RuntimeMin)
	}
	if movie.IMDBID != "tt0133093" {
		t.Errorf("imdb = %q", movie.IMDBID)
	}
	if movie.ReleasedAt == nil || !movie.ReleasedAt.Equal(time.Date(1999, 3, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("released = %v, want 1999-03-31", movie.ReleasedAt)
	}
}

const tvBody = `{
  "id": 1399,
  "name": "Игра престолов",
  "original_name": "Game of Thrones",
  "poster_path": "/got.jpg",
  "first_air_date": "2011-04-17",
  "episode_run_time": [60],
  "seasons": [
    {"season_number": 0, "name": "Спецвыпуски", "episode_count": 5, "air_date": "2010-01-01"},
    {"season_number": 1, "name": "Сезон 1", "episode_count": 10, "air_date": "2011-04-17"},
    {"season_number": 2, "name": "Сезон 2", "episode_count": 10, "air_date": "2012-04-01"}
  ]
}`

// TestTVKeepsSeasonZero covers the reason MediaRef.Season is a pointer: TMDB
// numbers specials as season 0, so a sentinel-based representation would
// silently merge them with "no season at all".
func TestTVKeepsSeasonZero(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tvBody))
	})

	tv, err := c.TV(context.Background(), 1399)
	if err != nil {
		t.Fatalf("TV: %v", err)
	}

	if len(tv.Seasons) != 3 {
		t.Fatalf("got %d seasons, want 3", len(tv.Seasons))
	}
	if tv.Seasons[0].Number != 0 {
		t.Errorf("first season number = %d, want 0 (specials)", tv.Seasons[0].Number)
	}
	if tv.RuntimeMin != 60 {
		t.Errorf("runtime = %d, want 60 from episode_run_time", tv.RuntimeMin)
	}
	if tv.Ref.Type != domain.MediaTypeTV {
		t.Errorf("ref type = %q", tv.Ref.Type)
	}
	// A series as a whole is not a request target.
	if tv.Ref.Season != nil {
		t.Error("a series detail must not carry a season")
	}
}

const seasonBody = `{
  "season_number": 2,
  "name": "Сезон 2",
  "air_date": "2012-04-01",
  "poster_path": "/got-s2.jpg",
  "episodes": [
    {"episode_number": 1, "name": "The North Remembers", "air_date": "2012-04-01", "runtime": 53},
    {"episode_number": 2, "name": "The Night Lands", "air_date": "2012-04-08", "runtime": 54},
    {"episode_number": 3, "name": "What Is Dead May Never Die", "air_date": "2012-04-15", "runtime": 55},
    {"episode_number": 4, "name": "Garden of Bones", "air_date": "2099-01-01", "runtime": 56}
  ]
}`

func TestSeasonAiredCount(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/1399/season/2" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(seasonBody))
	})

	season, err := c.Season(context.Background(), 1399, 2)
	if err != nil {
		t.Fatalf("Season: %v", err)
	}

	if season.Number != 2 || season.EpisodeCount != 4 {
		t.Fatalf("season = %d with %d episodes", season.Number, season.EpisodeCount)
	}

	// The fourth episode airs in the future, so three have aired. This is what
	// stops the search loop from hammering indexers for an unaired season.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := season.AiredCount(now); got != 3 {
		t.Errorf("AiredCount = %d, want 3", got)
	}

	newest := season.NewestAirDate()
	if newest == nil || newest.Year() != 2099 {
		t.Errorf("NewestAirDate = %v, want the 2099 episode", newest)
	}
}

// TestToSpecOnlyRecordsRussianWhenLocalised covers a silent-fallback trap:
// when TMDB has no translation it returns the original string with no flag
// saying so. Recording that as title_ru would put an English name where the
// UI promises Russian, and would make the release selector prefer Russian
// releases for a title nobody asked for in Russian.
func TestToSpecOnlyRecordsRussianWhenLocalised(t *testing.T) {
	localised := Title{
		Ref:           domain.MovieRef(603),
		Title:         "Матрица",
		OriginalTitle: "The Matrix",
		Year:          1999,
	}
	if got := localised.ToSpec().TitleRU; got != "Матрица" {
		t.Errorf("title_ru = %q, want the localised title", got)
	}

	untranslated := Title{
		Ref:           domain.MovieRef(999),
		Title:         "Undated Film",
		OriginalTitle: "Undated Film",
	}
	if got := untranslated.ToSpec().TitleRU; got != "" {
		t.Errorf("title_ru = %q, want empty when TMDB fell back to the original", got)
	}
}

func TestPosterURL(t *testing.T) {
	// poster_path is nullable, and it is genuinely absent for obscure titles.
	if got := PosterURL("", "w500"); got != "" {
		t.Errorf("PosterURL(\"\") = %q, want empty", got)
	}
	if got := PosterURL("/x.jpg", "w500"); got != "https://image.tmdb.org/t/p/w500/x.jpg" {
		t.Errorf("PosterURL = %q", got)
	}
	if got := PosterURL("/x.jpg", ""); !strings.Contains(got, "/w500/") {
		t.Errorf("PosterURL with no size = %q, want a default size", got)
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status      int
		rateLimited bool
		unauth      bool
	}{
		{http.StatusTooManyRequests, true, false},
		{http.StatusUnauthorized, false, true},
		{http.StatusNotFound, false, false},
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"status_message":"nope"}`))
		})

		_, err := c.Movie(context.Background(), 1)
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}

		var apiErr *Error
		if !asError(err, &apiErr) {
			t.Fatalf("status %d: error is %T, want *Error", tc.status, err)
		}
		if apiErr.RateLimited() != tc.rateLimited {
			t.Errorf("status %d: RateLimited = %v", tc.status, apiErr.RateLimited())
		}
		if apiErr.Unauthorized() != tc.unauth {
			t.Errorf("status %d: Unauthorized = %v", tc.status, apiErr.Unauthorized())
		}
	}
}

func TestRetryAfterIsParsed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := c.Movie(context.Background(), 1)

	var apiErr *Error
	if !asError(err, &apiErr) {
		t.Fatalf("error is %T, want *Error", err)
	}
	if apiErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", apiErr.RetryAfter)
	}
}

func TestAuthStyleIsInferredFromKeyShape(t *testing.T) {
	// A v4 token is a JWT and has two dots; a v3 key does not. Sending the
	// wrong style produces a 401 that says nothing about which was expected.
	v4 := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.signature"
	if strings.Count(v4, ".") != 2 {
		t.Fatal("test fixture is not shaped like a JWT")
	}

	cfg := &config.Config{}
	cfg.TMDB.APIKey = v4

	c, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.bearer == "" || c.apiKey != "" {
		t.Errorf("a JWT-shaped key should be used as a bearer token, got bearer=%q apiKey=%q", c.bearer, c.apiKey)
	}
}

func TestMissingAPIKeyFailsClearly(t *testing.T) {
	cfg := &config.Config{}
	c, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New must not fail without a key: %v", err)
	}

	_, err = c.SearchMulti(context.Background(), "anything")
	if err == nil || !strings.Contains(err.Error(), "api key") {
		t.Errorf("error = %v, want one naming the missing api key", err)
	}
}

// asError is errors.As specialised to *Error, kept local to avoid importing
// errors in every test.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// TestJSONShapesAreStable guards the wire structs against a careless edit:
// these are the exact field names TMDB uses.
func TestJSONShapesAreStable(t *testing.T) {
	var raw rawSearchItem
	if err := json.Unmarshal([]byte(`{"media_type":"movie","id":1,"title":"T","release_date":"2020-01-01"}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.MediaType != "movie" || raw.ID != 1 || raw.Title != "T" || raw.ReleaseDate != "2020-01-01" {
		t.Errorf("decoded %+v", raw)
	}
}
