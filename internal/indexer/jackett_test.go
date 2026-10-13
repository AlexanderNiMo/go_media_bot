package indexer

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/alex/mediad/internal/config"
)

func newTestJackett(t *testing.T, handler http.HandlerFunc) *Jackett {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Indexer.Mode = "jackett"
	cfg.Indexer.BaseURL = srv.URL
	cfg.Indexer.APIKey = "test-key"
	cfg.Indexer.RatePerSecond = 1000

	return NewJackett(cfg, slog.New(slog.DiscardHandler))
}

// TestJackettCapsReflectTheAggregate makes the narrow aggregate contract
// explicit: only free-text search is supported, and any other capability
// must read as unavailable so a workflow that asks for imdbid or tvdbid
// does not silently match nothing.
func TestJackettCapsReflectTheAggregate(t *testing.T) {
	j := newTestJackett(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Caps must not contact the indexer, got request %q", r.URL.String())
	})

	caps, err := j.Caps(context.Background())
	if err != nil {
		t.Fatalf("Caps: %v", err)
	}
	if !caps.Search.Available || !caps.Search.Supports("q") {
		t.Errorf("search caps = %+v, want available with q", caps.Search)
	}
	if caps.MovieQuery.Available {
		t.Errorf("movie caps = %+v, want unavailable", caps.MovieQuery)
	}
	if caps.TVQuery.Available {
		t.Errorf("tv caps = %+v, want unavailable", caps.TVQuery)
	}
}

// TestJackettSearchUsesFreeText pins the wire format: the aggregate accepts
// only `t=search`, and the client must send exactly that with a non-empty
// `q`, regardless of whether the spec carries an IMDB id or a season.
func TestJackettSearchUsesFreeText(t *testing.T) {
	var got url.Values
	j := newTestJackett(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		got = r.URL.Query()
		_, _ = w.Write([]byte(searchFeed))
	})

	rels, err := j.Search(context.Background(), SearchRequest{
		Query:  "Матрица",
		IMDBID: "tt0133093",
		Season: ptr(1),
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rels) == 0 {
		t.Fatalf("releases = 0, want the test fixture to round-trip")
	}
	if got.Get("t") != "search" {
		t.Errorf("t = %q, want search", got.Get("t"))
	}
	if got.Get("q") != "Матрица tt0133093" {
		t.Errorf("q = %q, want %q", got.Get("q"), "Матрица tt0133093")
	}
	if got.Get("extended") != "1" {
		t.Errorf("extended = %q, want 1", got.Get("extended"))
	}
}

// TestJackettEndToEnd makes sure the wire format the unit test pins actually
// produces releases when the response is a real Jackett document — the
// fixture in searchFeed is the same one the other clients use, so this is
// the closest we can get to "the client talks to a real Jackett" without
// pulling one into the test suite.
func TestJackettEndToEnd(t *testing.T) {
	j := newTestJackett(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") != "search" {
			t.Errorf("t = %q, want search", r.URL.Query().Get("t"))
		}
		_, _ = w.Write([]byte(searchFeed))
	})

	rels, err := j.Search(context.Background(), SearchRequest{
		Query: "Матрица",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rels) == 0 {
		t.Fatalf("no releases parsed back from the fixture")
	}
	// At least one of the fixtures carries an infohash; the absence of all of
	// them would mean the extended attributes were dropped on the floor.
	foundHash := false
	for _, r := range rels {
		if r.InfoHash != "" {
			foundHash = true
			break
		}
	}
	if !foundHash {
		t.Errorf("no release has an infohash — extended attributes were stripped")
	}
}

func ptr[T any](v T) *T { return &v }
