package plex

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Plex.URL = srv.URL
	cfg.Plex.Token = "test-token"
	cfg.Plex.SectionsTTL = 0 // never serve a stale section list between cases

	return New(cfg, slog.New(slog.DiscardHandler))
}

const sectionsBody = `{
  "MediaContainer": {
    "Directory": [
      {"key": "1", "title": "Фильмы", "type": "movie"},
      {"key": "2", "title": "Сериалы", "type": "show"},
      {"key": "3", "title": "Музыка", "type": "artist"}
    ]
  }
}`

func TestSectionsFiltersByType(t *testing.T) {
	var sawAccept, sawToken string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sawAccept = r.Header.Get("Accept")
		sawToken = r.Header.Get("X-Plex-Token")
		_, _ = w.Write([]byte(sectionsBody))
	})

	sections, err := c.Sections(context.Background())
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}

	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(sections))
	}

	// Without an explicit Accept the server answers XML, which unmarshals into
	// an empty struct rather than failing — a silent wrong answer.
	if sawAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", sawAccept)
	}
	if sawToken != "test-token" {
		t.Errorf("X-Plex-Token = %q", sawToken)
	}
}

// movieSectionBody holds a film whose title matches but whose guid does not,
// alongside the film actually being looked for. A title-based match would
// return the wrong one.
const movieSectionBody = `{
  "MediaContainer": {
    "Metadata": [
      {
        "ratingKey": "900",
        "title": "Матрица",
        "year": 2024,
        "Guid": [{"id": "tmdb://999999"}, {"id": "imdb://tt9999999"}]
      },
      {
        "ratingKey": "901",
        "title": "The Matrix",
        "year": 1999,
        "Guid": [{"id": "tmdb://603"}, {"id": "imdb://tt0133093"}]
      }
    ]
  }
}`

func TestFindMovieMatchesOnGUIDNotTitle(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/sections":
			_, _ = w.Write([]byte(sectionsBody))
		case strings.Contains(r.URL.Path, "/library/sections/1/all"):
			if !strings.Contains(r.URL.RawQuery, "includeGuids=1") {
				t.Error("includeGuids was not requested; guids cannot be matched without it")
			}
			if got := r.URL.Query().Get("type"); got != "1" {
				t.Errorf("type = %q, want 1 for a movie section", got)
			}
			_, _ = w.Write([]byte(movieSectionBody))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	match, err := c.FindMovie(context.Background(), domain.MovieRef(603), "Матрица")
	if err != nil {
		t.Fatalf("FindMovie: %v", err)
	}
	if match == nil {
		t.Fatal("the film is in the library but was not found")
	}

	// The decoy shares the title and must not win; the real one is matched by
	// guid even though its title is the English one.
	if match.RatingKey != "901" {
		t.Errorf("matched ratingKey %q, want 901 — the title-matching decoy was picked", match.RatingKey)
	}
	if match.SectionKey != "1" {
		t.Errorf("section = %q, want 1", match.SectionKey)
	}
	if id, ok := match.TMDBID(); !ok || id != 603 {
		t.Errorf("TMDBID = %d/%v, want 603/true", id, ok)
	}
	if match.Year != 1999 {
		t.Errorf("year = %d, want 1999", match.Year)
	}
}

func TestFindMovieReturnsNilWhenAbsent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(movieSectionBody))
	})

	// Not in the library, and the film section is the only one consulted.
	match, err := c.FindMovie(context.Background(), domain.MovieRef(27205), "Начало")
	if err != nil {
		t.Fatalf("FindMovie: %v", err)
	}
	if match != nil {
		t.Errorf("found %+v for a film that is not in the library", match)
	}
}

const showSectionBody = `{
  "MediaContainer": {
    "Metadata": [
      {
        "ratingKey": "500",
        "title": "Игра престолов",
        "year": 2011,
        "Guid": [{"id": "tmdb://1399"}, {"id": "tvdb://121361"}]
      }
    ]
  }
}`

func TestFindSeasonLocatesChildByIndex(t *testing.T) {
	var childrenPath string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/sections":
			_, _ = w.Write([]byte(sectionsBody))

		case strings.Contains(r.URL.Path, "/library/sections/2/all"):
			if got := r.URL.Query().Get("type"); got != "2" {
				t.Errorf("type = %q, want 2 for a show section", got)
			}
			_, _ = w.Write([]byte(showSectionBody))

		case strings.Contains(r.URL.Path, "/library/metadata/500/children"):
			childrenPath = r.URL.Path
			// Plex attaches the tmdb:// guid to the show, not the season, so
			// the season is matched on index among the show's children.
			_, _ = w.Write([]byte(`{
              "MediaContainer": {
                "Metadata": [
                  {"ratingKey": "501", "index": 1, "leafCount": 10, "addedAt": 1600000000},
                  {"ratingKey": "502", "index": 2, "leafCount": 10, "addedAt": 1610000000},
                  {"ratingKey": "503", "index": 3, "leafCount": 7, "addedAt": 1620000000}
                ]
              }
            }`))

		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	match, err := c.FindSeason(context.Background(), domain.SeasonRef(1399, 2), "Игра престолов")
	if err != nil {
		t.Fatalf("FindSeason: %v", err)
	}
	if match == nil {
		t.Fatal("season 2 is in the library but was not found")
	}

	if childrenPath == "" {
		t.Error("the show's children were never listed")
	}
	if match.RatingKey != "502" {
		t.Errorf("ratingKey = %q, want 502 (season 2)", match.RatingKey)
	}
	if match.LeafCount != 10 {
		t.Errorf("leafCount = %d, want 10 — this is the baseline the post-download check compares against", match.LeafCount)
	}
	if match.AddedAt.IsZero() {
		t.Error("addedAt was not decoded")
	}
}

func TestFindSeasonReturnsNilWhenSeasonAbsent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/sections":
			_, _ = w.Write([]byte(sectionsBody))
		case strings.Contains(r.URL.Path, "/library/sections/2/all"):
			_, _ = w.Write([]byte(showSectionBody))
		default:
			// The show is present but has no season 9.
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"501","index":1,"leafCount":10}]}}`))
		}
	})

	match, err := c.FindSeason(context.Background(), domain.SeasonRef(1399, 9), "Игра престолов")
	if err != nil {
		t.Fatalf("FindSeason: %v", err)
	}
	if match != nil {
		t.Errorf("found %+v for a season that does not exist", match)
	}
}

// TestEpisodeCountIsZeroWhenAbsent — the count is snapshotted before a
// download and compared after, so "absent" has to be a plain zero rather than
// an error the caller has to special-case.
func TestEpisodeCountIsZeroWhenAbsent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})

	got, err := c.EpisodeCount(context.Background(), domain.SeasonRef(1399, 2), "Игра престолов")
	if err != nil {
		t.Fatalf("EpisodeCount: %v", err)
	}
	if got != 0 {
		t.Errorf("EpisodeCount = %d, want 0", got)
	}
}

func TestFindSeasonRequiresASeason(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})

	if _, err := c.FindSeason(context.Background(), domain.MovieRef(603), "x"); err == nil {
		t.Fatal("expected an error when no season is given")
	}
}

func TestRefreshSectionPassesPath(t *testing.T) {
	var gotPath, gotForce string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		gotForce = r.URL.Query().Get("force")
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":0}}`))
	})

	err := c.RefreshSection(context.Background(), "1", "/media/Movies/The Matrix (1999)")
	if err != nil {
		t.Fatalf("RefreshSection: %v", err)
	}

	// The path is what makes this cheap: without it Plex rescans the whole
	// library on every completed download.
	if gotPath != "/media/Movies/The Matrix (1999)" {
		t.Errorf("path = %q", gotPath)
	}
	if gotForce != "0" {
		t.Errorf("force = %q, want 0 so Plex uses its own change detection", gotForce)
	}
}

func TestUnauthorizedIsRecognised(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := c.Sections(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}

	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if !apiErr.Unauthorized() {
		t.Error("a 401 should be recognised as an auth failure")
	}
}

// TestBrokenSectionDoesNotFailTheCheck — a library being scanned returns
// errors that have nothing to do with the title being looked for, and must not
// make the whole existence check fail.
func TestBrokenSectionDoesNotFailTheCheck(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		if strings.Contains(r.URL.Path, "/library/sections/1/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})

	match, err := c.FindMovie(context.Background(), domain.MovieRef(603), "Матрица")
	if err != nil {
		t.Fatalf("a failing section must not fail the check: %v", err)
	}
	if match != nil {
		t.Errorf("found %+v", match)
	}
}

func TestNotConfigured(t *testing.T) {
	c := New(&config.Config{}, slog.New(slog.DiscardHandler))
	if c.Configured() {
		t.Error("a client with no url or token must report itself unconfigured")
	}
	if _, err := c.FindMovie(context.Background(), domain.MovieRef(1), "x"); err == nil {
		t.Fatal("expected an error from an unconfigured client")
	}
}

// TestGUIDListAcceptsBothShapes — the field is "Guid" on newer servers and
// "guid" on older ones, and both spellings appear in the same document.
func TestGUIDListAcceptsBothShapes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{
          "MediaContainer": {
            "Metadata": [
              {"ratingKey": "1", "title": "X", "Guid": [{"id": "tmdb://603"}]},
              {"ratingKey": "2", "title": "Y", "guid": [{"id": "tmdb://604"}]}
            ]
          }
        }`))
	})

	for _, tc := range []struct {
		id   int64
		want string
	}{{603, "1"}, {604, "2"}} {
		match, err := c.FindMovie(context.Background(), domain.MovieRef(tc.id), "x")
		if err != nil {
			t.Fatalf("tmdb %d: %v", tc.id, err)
		}
		if match == nil || match.RatingKey != tc.want {
			t.Errorf("tmdb %d: match = %+v, want ratingKey %s", tc.id, match, tc.want)
		}
	}
}
