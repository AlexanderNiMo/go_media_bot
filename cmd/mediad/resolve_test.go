package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/prompt"
	"github.com/alex/mediad/internal/tmdb"
)

// The resolution flow is where a wrong choice becomes a two-year download, so
// the cases that matter are the ambiguous ones: more than one title, a series
// whose season has to be picked, and a command run from a script where there
// is nobody to ask.

// stubLookup answers with canned metadata.
type stubLookup struct {
	results []tmdb.SearchResult
	titles  map[int64]*tmdb.Title
	err     error

	searched []string
	loaded   []int64
}

func (s *stubLookup) SearchMulti(_ context.Context, query string) ([]tmdb.SearchResult, error) {
	s.searched = append(s.searched, query)
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func (s *stubLookup) Movie(_ context.Context, tmdbID int64) (*tmdb.Title, error) {
	s.loaded = append(s.loaded, tmdbID)
	if s.err != nil {
		return nil, s.err
	}
	return s.titles[tmdbID], nil
}

func (s *stubLookup) TV(_ context.Context, tmdbID int64) (*tmdb.Title, error) {
	s.loaded = append(s.loaded, tmdbID)
	if s.err != nil {
		return nil, s.err
	}
	return s.titles[tmdbID], nil
}

func movieResult(id int64, title string, year int) tmdb.SearchResult {
	return tmdb.SearchResult{
		Ref:   domain.MovieRef(id),
		Title: title,
		Year:  year,
	}
}

func tvResult(id int64, title string, year int) tmdb.SearchResult {
	return tmdb.SearchResult{
		Ref:   domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: id},
		Title: title,
		Year:  year,
	}
}

func testMovie(id int64, title, original string, year int) *tmdb.Title {
	return &tmdb.Title{
		Ref:           domain.MovieRef(id),
		Title:         title,
		OriginalTitle: original,
		Year:          year,
		RuntimeMin:    136,
		IMDBID:        "tt0133093",
	}
}

func testShow(id int64, title string, seasons ...tmdb.Season) *tmdb.Title {
	return &tmdb.Title{
		Ref:           domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: id},
		Title:         title,
		OriginalTitle: "Game of Thrones",
		Year:          2011,
		Seasons:       seasons,
	}
}

// promptFor builds a prompter reading the given answers, with its output
// captured so a test can assert what the user was actually shown.
func promptFor(answers string) (*prompt.Prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return prompt.New(strings.NewReader(answers), out), out
}

// ---------------------------------------------------------------------------
// Choosing among the results
// ---------------------------------------------------------------------------

func TestResolveSearchTakesThePickedResult(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{
			movieResult(603, "Матрица", 1999),
			movieResult(604, "Матрица: Перезагрузка", 2003),
		},
		titles: map[int64]*tmdb.Title{604: testMovie(604, "Матрица: Перезагрузка", "The Matrix Reloaded", 2003)},
	}

	// A nil prompter: --pick means no question is asked, and a test that
	// passed one would not notice if that stopped being true.
	spec, err := resolveSearch(context.Background(), lookup, nil, searchOptions{Query: "матрица", Pick: 2})
	if err != nil {
		t.Fatalf("resolveSearch: %v", err)
	}

	if spec.Ref.TMDBID != 604 {
		t.Errorf("picked tmdb %d, want 604", spec.Ref.TMDBID)
	}
	if spec.Title != "Матрица: Перезагрузка" {
		t.Errorf("title = %q", spec.Title)
	}
	if spec.RuntimeMin != 136 || spec.IMDBID != "tt0133093" {
		t.Errorf("the metadata was not loaded from the title: %+v", spec)
	}
}

func TestResolveSearchAsksWhichOne(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{
			movieResult(603, "Матрица", 1999),
			movieResult(604, "Матрица: Перезагрузка", 2003),
			movieResult(605, "Матрица: Революция", 2003),
		},
		titles: map[int64]*tmdb.Title{605: testMovie(605, "Матрица: Революция", "The Matrix Revolutions", 2003)},
	}

	p, out := promptFor("3\n")

	spec, err := resolveSearch(context.Background(), lookup, p, searchOptions{Query: "матрица"})
	if err != nil {
		t.Fatalf("resolveSearch: %v", err)
	}
	if spec.Ref.TMDBID != 605 {
		t.Errorf("picked tmdb %d, want 605", spec.Ref.TMDBID)
	}

	// The menu has to show enough to tell the three apart: the year and the
	// kind are what distinguish them, and the workflow id is what the user
	// will see again in `list`.
	printed := out.String()
	for _, want := range []string{"Матрица (1999)", "Матрица: Перезагрузка (2003)", "movie-605"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the menu is missing %q; it printed:\n%s", want, printed)
		}
	}
}

func TestResolveSearchWithoutATerminalNamesTheFlag(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{
			movieResult(603, "Матрица", 1999),
			movieResult(604, "Матрица: Перезагрузка", 2003),
		},
	}

	_, err := resolveSearch(context.Background(), lookup, nil, searchOptions{Query: "матрица"})
	if err == nil {
		t.Fatal("expected an error when there is nobody to ask")
	}
	if !strings.Contains(err.Error(), "--pick") {
		t.Errorf("the error should say what to pass instead, got %v", err)
	}
}

// TestResolveSearchConfirmsASingleMatch — one result is unambiguous enough for
// a script, but a person is still asked, because a common word can match
// exactly one wrong film and this is the last moment before a two-year
// commitment.
func TestResolveSearchConfirmsASingleMatch(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{movieResult(603, "Матрица", 1999)},
		titles:  map[int64]*tmdb.Title{603: testMovie(603, "Матрица", "The Matrix", 1999)},
	}

	t.Run("accepted", func(t *testing.T) {
		p, out := promptFor("y\n")

		spec, err := resolveSearch(context.Background(), lookup, p, searchOptions{Query: "матрица"})
		if err != nil {
			t.Fatalf("resolveSearch: %v", err)
		}
		if spec.Ref.TMDBID != 603 {
			t.Errorf("picked tmdb %d, want 603", spec.Ref.TMDBID)
		}

		// What is being confirmed has to say which title, or the question is
		// not answerable.
		printed := out.String()
		for _, want := range []string{"will request:", "Матрица (1999)", "movie-603"} {
			if !strings.Contains(printed, want) {
				t.Errorf("the confirmation is missing %q; it printed:\n%s", want, printed)
			}
		}
	})

	t.Run("declined", func(t *testing.T) {
		p, _ := promptFor("n\n")

		if _, err := resolveSearch(context.Background(), lookup, p, searchOptions{Query: "матрица"}); err == nil {
			t.Fatal("expected the decline to stop the command")
		} else if !strings.Contains(err.Error(), prompt.ErrAborted.Error()) {
			t.Errorf("want an abort, got %v", err)
		}
	})

	t.Run("no terminal", func(t *testing.T) {
		spec, err := resolveSearch(context.Background(), lookup, nil, searchOptions{Query: "матрица"})
		if err != nil {
			t.Fatalf("an unambiguous result should need no confirmation: %v", err)
		}
		if spec.Ref.TMDBID != 603 {
			t.Errorf("picked tmdb %d, want 603", spec.Ref.TMDBID)
		}
	})

	t.Run("picked by flag", func(t *testing.T) {
		// --pick is the user saying which one, so there is nothing left to
		// confirm and the prompter must not be read from at all.
		p, out := promptFor("")

		spec, err := resolveSearch(context.Background(), lookup, p,
			searchOptions{Query: "матрица", Pick: 1})
		if err != nil {
			t.Fatalf("resolveSearch: %v", err)
		}
		if spec.Ref.TMDBID != 603 {
			t.Errorf("picked tmdb %d, want 603", spec.Ref.TMDBID)
		}
		if strings.Contains(out.String(), "will request") {
			t.Errorf("a flag already decided it, so nothing should have been asked:\n%s", out)
		}
	})
}

func TestResolveSearchWithNothingFound(t *testing.T) {
	lookup := &stubLookup{}

	_, err := resolveSearch(context.Background(), lookup, nil, searchOptions{Query: "фывапролдж"})
	if err == nil || !strings.Contains(err.Error(), "nothing on TMDB matches") {
		t.Fatalf("want a nothing-found error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Seasons
// ---------------------------------------------------------------------------

func gameOfThrones() *tmdb.Title {
	premiere := time.Date(2011, time.April, 17, 0, 0, 0, 0, time.UTC)
	season2 := time.Date(2012, time.April, 1, 0, 0, 0, 0, time.UTC)
	season6 := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)

	return testShow(1399, "Игра престолов",
		tmdb.Season{Number: 1, Name: "Season 1", EpisodeCount: 10, AirDate: &premiere},
		tmdb.Season{Number: 2, Name: "Season 2", EpisodeCount: 10, AirDate: &season2},
		tmdb.Season{Number: 6, Name: "Season 6", EpisodeCount: 10, AirDate: &season6},
	)
}

func TestResolveSearchPicksASeason(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{tvResult(1399, "Игра престолов", 2011)},
		titles:  map[int64]*tmdb.Title{1399: gameOfThrones()},
	}

	// One answer only: the season menu. Having chosen, the user has confirmed,
	// so there is no second question — and a test that supplied a second
	// answer would not notice if one crept back in.
	p, out := promptFor("2\n")

	spec, err := resolveSearch(context.Background(), lookup, p, searchOptions{Query: "игра престолов"})
	if err != nil {
		t.Fatalf("resolveSearch: %v", err)
	}

	if !spec.Ref.IsSeason() || spec.Ref.SeasonNumber() != 2 {
		t.Fatalf("ref = %s, want season 2", spec.Ref)
	}
	if spec.Ref.Type != domain.MediaTypeTV {
		t.Errorf("media type = %q", spec.Ref.Type)
	}

	// The season list is what the choice is made from, so it has to carry the
	// facts that distinguish seasons: the number, the episode count and the
	// date.
	printed := out.String()
	for _, want := range []string{"сезон 1", "сезон 6", "10 эпизодов", "2012-04-01"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the season menu is missing %q; it printed:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "will request") {
		t.Errorf("the season choice was already an answer; nothing more should be asked:\n%s", printed)
	}
}

// TestSeasonSpecUsesTheSeasonAirDate is the difference between searching for a
// season that exists and searching for one that does not.
func TestSeasonSpecUsesTheSeasonAirDate(t *testing.T) {
	show := gameOfThrones()
	spec := seasonSpec(show, show.Seasons[1])

	// A season's première, not the series': the series began in 2011, and a
	// request carrying that date would tell the workflow the media has been
	// out for fifteen years.
	if spec.ReleasedAt == nil {
		t.Fatal("the season air date was dropped")
	}
	if got := spec.ReleasedAt.Format(time.DateOnly); got != "2012-04-01" {
		t.Errorf("released at %s, want the season's première 2012-04-01", got)
	}
	if spec.Year != 2011 {
		t.Errorf("year = %d, want the series' 2011", spec.Year)
	}
}

func TestSeasonSpecForASeasonWithNoDate(t *testing.T) {
	show := testShow(1399, "Игра престолов",
		tmdb.Season{Number: 8, EpisodeCount: 6}, // no air date yet
	)

	spec := seasonSpec(show, show.Seasons[0])

	if !spec.Ref.IsSeason() || spec.Ref.SeasonNumber() != 8 {
		t.Fatalf("ref = %s", spec.Ref)
	}
	if spec.ReleasedAt != nil {
		// Falling back to the series' date would be worse than nothing: the
		// workflow treats a known past date as "released, search now", which
		// is exactly wrong for a season that has not aired.
		t.Errorf("released at %v, want nil so the workflow searches rather than assuming", spec.ReleasedAt)
	}
}

func TestResolveSearchRequiresASeason(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{tvResult(1399, "Игра престолов", 2011)},
		titles:  map[int64]*tmdb.Title{1399: gameOfThrones()},
	}

	// Unambiguous series, but there is no terminal to ask which season.
	_, err := resolveSearch(context.Background(), lookup, nil, searchOptions{Query: "игра престолов"})
	if err == nil {
		t.Fatal("expected an error when the season cannot be chosen")
	}
	for _, want := range []string{"--season", "1, 2, 6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got %v", want, err)
		}
	}
}

func TestResolveSearchRejectsASeasonThatDoesNotExist(t *testing.T) {
	lookup := &stubLookup{
		results: []tmdb.SearchResult{tvResult(1399, "Игра престолов", 2011)},
		titles:  map[int64]*tmdb.Title{1399: gameOfThrones()},
	}

	missing := 4
	_, err := resolveSearch(context.Background(), lookup, nil,
		searchOptions{Query: "игра престолов", Season: &missing})
	if err == nil {
		t.Fatal("expected an error for a season the series does not have")
	}
	if !strings.Contains(err.Error(), "no season 4") {
		t.Errorf("the error should say which season is missing, got %v", err)
	}
}

func TestResolveSearchAcceptsSeasonZero(t *testing.T) {
	// TMDB numbers specials as season 0, so the zero value cannot mean
	// "not given" anywhere in this path.
	show := testShow(1399, "Игра престолов", tmdb.Season{Number: 0, EpisodeCount: 3})

	lookup := &stubLookup{
		results: []tmdb.SearchResult{tvResult(1399, "Игра престолов", 2011)},
		titles:  map[int64]*tmdb.Title{1399: show},
	}

	zero := 0
	spec, err := resolveSearch(context.Background(), lookup, nil,
		searchOptions{Query: "игра престолов", Season: &zero})
	if err != nil {
		t.Fatalf("resolveSearch: %v", err)
	}
	if spec.Ref.SeasonNumber() != 0 || !spec.Ref.IsSeason() {
		t.Errorf("ref = %s, want season 0", spec.Ref)
	}
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

func TestResolveByID(t *testing.T) {
	lookup := &stubLookup{
		titles: map[int64]*tmdb.Title{603: testMovie(603, "Матрица", "The Matrix", 1999)},
	}

	spec, err := resolveByID(context.Background(), lookup, domain.MovieRef(603), nil, nil)
	if err != nil {
		t.Fatalf("resolveByID: %v", err)
	}
	if spec.Ref.TMDBID != 603 || spec.Ref.Type != domain.MediaTypeMovie {
		t.Errorf("ref = %s", spec.Ref)
	}
}

// TestResolveByIDRejectsASeriesWithoutASeason — the same rule the workflow
// enforces, applied before anything is written down.
func TestResolveByIDRejectsASeriesWithoutASeason(t *testing.T) {
	lookup := &stubLookup{titles: map[int64]*tmdb.Title{1399: gameOfThrones()}}

	ref := domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: 1399}

	_, err := resolveByID(context.Background(), lookup, ref, nil, nil)
	if err == nil {
		t.Fatal("expected a series without a season to be refused")
	}
	if !strings.Contains(err.Error(), "сезон") && !strings.Contains(err.Error(), "season") {
		t.Errorf("the error should explain that a season is needed, got %v", err)
	}
}

// TestResolveByIDWhenTheClientReturnsNothing — the client reporting success
// and no title is a broken contract, and it has to be caught where it happens
// rather than at the dereference.
func TestResolveByIDWhenTheClientReturnsNothing(t *testing.T) {
	lookup := &stubLookup{titles: map[int64]*tmdb.Title{}}

	_, err := resolveByID(context.Background(), lookup, domain.MovieRef(603), nil, nil)
	if err == nil {
		t.Fatal("expected an error when the metadata service returns no title")
	}
	if !strings.Contains(err.Error(), "returned nothing") {
		t.Errorf("want an error naming the empty response, got %v", err)
	}
}

func TestResolveByIDForASeries(t *testing.T) {
	lookup := &stubLookup{titles: map[int64]*tmdb.Title{1399: gameOfThrones()}}

	season := 2
	ref := domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: 1399}

	spec, err := resolveByID(context.Background(), lookup, ref, nil, &season)
	if err != nil {
		t.Fatalf("resolveByID: %v", err)
	}
	if spec.Ref.SeasonNumber() != 2 || !spec.Ref.IsSeason() {
		t.Errorf("ref = %s, want season 2", spec.Ref)
	}
}

func TestResolveByIDRejectsABadReference(t *testing.T) {
	lookup := &stubLookup{}

	if _, err := resolveByID(context.Background(), lookup, domain.MovieRef(0), nil, nil); err == nil {
		t.Fatal("expected a zero tmdb id to be refused before any request")
	}
	if _, err := resolveByID(context.Background(), lookup,
		domain.MovieRef(603), nil, ptr(2)); err == nil {
		t.Fatal("expected a film with a season to be refused")
	}
}
