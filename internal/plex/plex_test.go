package plex

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestGUIDListAcceptsBothShapes — the live Plex response carries both the
// lowercase "guid" string ("plex://movie/<ratingKey>") and a capitalised
// "Guid" array of external ids (tmdb://, imdb://). The decoder must accept
// both forms in the same document; an earlier version of this test assumed
// "guid" was itself a list, which is what Plex used to ship.
func TestGUIDListAcceptsBothShapes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{
          "MediaContainer": {
            "Metadata": [
              {
                "ratingKey": "1",
                "title": "X",
                "guid": "plex://movie/1",
                "Guid": [{"id": "tmdb://603"}]
              },
              {
                "ratingKey": "2",
                "title": "Y",
                "guid": "plex://movie/2",
                "Guid": []
              }
            ]
          }
        }`))
	})

	match, err := c.FindMovie(context.Background(), domain.MovieRef(603), "x")
	if err != nil {
		t.Fatalf("FindMovie: %v", err)
	}
	if match == nil || match.RatingKey != "1" {
		t.Errorf("match = %+v, want ratingKey 1", match)
	}
}

// TestFindMovieReadsBothGuidShapes pins what Plex actually returns: the
// internal "guid" string and the external "Guid" list. A match on the TMDB
// id (which lives in Guid, not guid) has to succeed even when guid is a
// string and not a list.
func TestFindMovieReadsBothGuidShapes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		// Verbatim shape from a live /library/sections/1/all response.
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {
		        "ratingKey": "5d776827880197001ec90904",
		        "title": "Матрица",
		        "year": 1999,
		        "guid": "plex://movie/5d776827880197001ec90904",
		        "Guid": []
		      },
		      {
		        "ratingKey": "5d776827a091de001f2e62c9",
		        "title": "Матрица: Перезагрузка",
		        "year": 2003,
		        "guid": "plex://movie/5d776827a091de001f2e62c9",
		        "Guid": [{"id": "tmdb://603"}]
		      }
		    ]
		  }
		}`))
	})

	match, err := c.FindMovie(context.Background(), domain.MovieRef(603), "Матрица")
	if err != nil {
		t.Fatalf("FindMovie: %v", err)
	}
	if match == nil || match.RatingKey != "5d776827a091de001f2e62c9" {
		t.Errorf("match = %+v, want ratingKey 5d776827a091de001f2e62c9", match)
	}
}

// TestFindMovieByTitleMatchesOnNormalizedTitle is the contract the
// application relies on: a library item whose Plex metadata is just
// "guid": "plex://..." with no external id is found by name+year, and a
// hand-renamed title ("The Matrix (1999)" vs "The Matrix") still matches.
func TestFindMovieByTitleMatchesOnNormalizedTitle(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		// Note: Guid is empty — the live case is items the user never tagged.
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "10", "title": "The Matrix (1999)", "year": 1999,
		       "guid": "plex://movie/10", "Guid": []},
		      {"ratingKey": "11", "title": "The Matrix", "year": 2021,
		       "guid": "plex://movie/11", "Guid": []}
		    ]
		  }
		}`))
	})

	match, err := c.FindMovieByTitle(context.Background(),
		[]string{"Матрица", "The Matrix"}, 1999)
	if err != nil {
		t.Fatalf("FindMovieByTitle: %v", err)
	}
	if match == nil || match.RatingKey != "10" {
		t.Fatalf("match = %+v, want ratingKey 10", match)
	}
}

// TestFindMovieByTitleRefusesToRunWithoutYear is the safety net: a year
// must always be supplied, because a title-only match would accept the
// wrong Matrix on every shared title. The error is a refusal rather than a
// silent fall-through.
func TestFindMovieByTitleRefusesToRunWithoutYear(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Plex must not be called: %q", r.URL.String())
	})
	_, err := c.FindMovieByTitle(context.Background(), []string{"Матрица"}, 0)
	if err == nil {
		t.Fatal("a title-only lookup must be rejected, not silently executed")
	}
}

// TestFindMovieByTitleRejectsYearMismatch is what guards against the
// "Matrix 1999 vs Matrix 2021" case in the live library. Both items exist
// there; matching by year keeps the answer correct.
func TestFindMovieByTitleRejectsYearMismatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "10", "title": "The Matrix", "year": 1999, "Guid": []},
		      {"ratingKey": "11", "title": "The Matrix", "year": 2021, "Guid": []}
		    ]
		  }
		}`))
	})

	match, err := c.FindMovieByTitle(context.Background(), []string{"The Matrix"}, 2021)
	if err != nil {
		t.Fatalf("FindMovieByTitle: %v", err)
	}
	if match == nil || match.RatingKey != "11" {
		t.Errorf("match = %+v, want ratingKey 11 (year 2021)", match)
	}
}

// TestFindMovieByTitleNormalisesPunctuation is the case that motivated the
// whole comparison: Plex users rename files freely, and a title with a
// trailing year or an exclamation mark has to land on the same item.
func TestFindMovieByTitleNormalisesPunctuation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "20", "title": "Матрица, 1999", "year": 1999, "Guid": []}
		    ]
		  }
		}`))
	})

	match, err := c.FindMovieByTitle(context.Background(), []string{"Матрица"}, 1999)
	if err != nil {
		t.Fatalf("FindMovieByTitle: %v", err)
	}
	if match == nil || match.RatingKey != "20" {
		t.Errorf("match = %+v, want ratingKey 20 (normalised title match)", match)
	}
}

// TestFindSeasonByTitleMatchesShow is the same path for series. Plex records
// the show's title and year, and the season is reached by index on the show's
// children.
func TestFindSeasonByTitleMatchesShow(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		if strings.Contains(r.URL.Path, "/library/sections/2/all") {
			_, _ = w.Write([]byte(`{
			  "MediaContainer": {
			    "Metadata": [
			      {"ratingKey": "30", "title": "Игра престолов", "year": 2011,
			       "Guid": []}
			    ]
			  }
			}`))
			return
		}
		// Children endpoint
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "31", "title": "Season 2", "index": 2, "leafCount": 10}
		    ]
		  }
		}`))
	})

	match, err := c.FindSeasonByTitle(context.Background(),
		[]string{"Игра престолов"}, 2011, 2)
	if err != nil {
		t.Fatalf("FindSeasonByTitle: %v", err)
	}
	if match == nil || match.RatingKey != "31" {
		t.Errorf("match = %+v, want ratingKey 31", match)
	}
	if match.LeafCount != 10 {
		t.Errorf("leafCount = %d, want 10", match.LeafCount)
	}
}

// TestFindSeasonByTitlePicksTheRequestedNumber is the regression that
// motivated the question. A show with seasons 1, 2 and 3 has to return
// the one named in the request, not the first child that happens to
// arrive from the server. Plex's children list is sorted by index, but
// "happens to arrive first" is a contract we should not be relying on.
func TestFindSeasonByTitlePicksTheRequestedNumber(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		if strings.Contains(r.URL.Path, "/library/sections/2/all") {
			_, _ = w.Write([]byte(`{
			  "MediaContainer": {
			    "Metadata": [
			      {"ratingKey": "show-30", "title": "Игра престолов", "year": 2011,
			       "Guid": []}
			    ]
			  }
			}`))
			return
		}
		// All three seasons present. Server returns them in index order, but
		// the test asserts a specific number is picked even if that changes.
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "s1", "title": "Season 1", "index": 1, "leafCount": 10},
		      {"ratingKey": "s2", "title": "Season 2", "index": 2, "leafCount": 10},
		      {"ratingKey": "s3", "title": "Season 3", "index": 3, "leafCount": 10}
		    ]
		  }
		}`))
	})

	for _, tc := range []struct {
		season int
		want   string
	}{
		{1, "s1"},
		{2, "s2"},
		{3, "s3"},
	} {
		t.Run(strconv.Itoa(tc.season), func(t *testing.T) {
			match, err := c.FindSeasonByTitle(context.Background(),
				[]string{"Игра престолов"}, 2011, tc.season)
			if err != nil {
				t.Fatalf("season %d: %v", tc.season, err)
			}
			if match == nil || match.RatingKey != tc.want {
				t.Errorf("season %d: match = %+v, want ratingKey %s", tc.season, match, tc.want)
			}
		})
	}
}

// TestFindSeasonByTitleReturnsNilWhenSeasonAbsent is the second half of the
// contract: a show is in the library, but the requested season is not, and
// the answer is "not in Plex" (nil), not an error. A wrong season number
// must not turn into a "skip the workflow" signal — the request still has
// to download that season.
func TestFindSeasonByTitleReturnsNilWhenSeasonAbsent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		if strings.Contains(r.URL.Path, "/library/sections/2/all") {
			_, _ = w.Write([]byte(`{
			  "MediaContainer": {
			    "Metadata": [
			      {"ratingKey": "show-40", "title": "Во все тяжкие", "year": 2008,
			       "Guid": []}
			    ]
			  }
			}`))
			return
		}
		// Only season 1 is in the library.
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "only-1", "title": "Season 1", "index": 1, "leafCount": 7}
		    ]
		  }
		}`))
	})

	match, err := c.FindSeasonByTitle(context.Background(),
		[]string{"Во все тяжкие"}, 2008, 2)
	if err != nil {
		t.Fatalf("FindSeasonByTitle: %v", err)
	}
	if match != nil {
		t.Errorf("season 2 is not in the library, but got match %+v", match)
	}
}

// TestFindSeasonByTitleRejectsWrongYearEvenWhenSeasonExists is the year
// anchor doing its job for series too: a different show with the same name
// must not match, even when its seasons look right. "Игра престолов" (the
// show) shares no name with anything, so the fixture uses a confusing
// title: "Доктор Кто" 2005 vs "Доктор Кто" 2024. The 2005 version is in
// the library; the request for 2024 must not pick it up.
func TestFindSeasonByTitleRejectsWrongYearEvenWhenSeasonExists(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			_, _ = w.Write([]byte(sectionsBody))
			return
		}
		_, _ = w.Write([]byte(`{
		  "MediaContainer": {
		    "Metadata": [
		      {"ratingKey": "doc-2005", "title": "Доктор Кто", "year": 2005, "Guid": []}
		    ]
		  }
		}`))
	})

	match, err := c.FindSeasonByTitle(context.Background(),
		[]string{"Доктор Кто"}, 2024, 1)
	if err != nil {
		t.Fatalf("FindSeasonByTitle: %v", err)
	}
	if match != nil {
		t.Errorf("year 2024 must not match the 2005 show, got %+v", match)
	}
}
