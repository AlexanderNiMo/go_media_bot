package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/prompt"
	"github.com/alex/mediad/internal/tmdb"
)

// Turning "матрица" into something requestable takes three steps, and only the
// first of them is a lookup. The other two are the user's: which of the six
// films called Матрица, and which season. Everything here is separate from the
// database and the workflow engine so that it can be tested against a stub
// metadata service, which is the only way to test the ambiguous cases that are
// the whole reason this code exists.

// mediaLookup is the part of the metadata client the CLI uses.
//
// An interface rather than *tmdb.Client so that the flows below can be driven
// by a stub. The concrete client satisfies it as it stands.
type mediaLookup interface {
	SearchMulti(ctx context.Context, query string) ([]tmdb.SearchResult, error)
	Movie(ctx context.Context, tmdbID int64) (*tmdb.Title, error)
	TV(ctx context.Context, tmdbID int64) (*tmdb.Title, error)
}

// searchOptions is what the flags said about a search.
type searchOptions struct {
	Query string
	// Pick is the one-based index into the search results, or 0 to ask.
	Pick int
	// Season is the season number, or nil to ask.
	Season *int
}

// resolver turns a query into a spec, keeping track of how much of it the user
// actually decided.
//
// That bookkeeping is the reason this is a type rather than three functions.
// The confirmation at the end exists for one case: a query that resolved
// without a single question being asked, where the tool is about to commit
// somebody to a two-year request on the strength of its own guess. When the
// user has already picked a title out of a menu or named a season, they have
// confirmed, and asking again is friction on the commonest path.
type resolver struct {
	lookup mediaLookup
	// p is nil when there is nobody to ask, which is the case in a script.
	// Every ambiguity then becomes an error naming the flag that would have
	// resolved it, rather than a menu read from a closed pipe.
	p *prompt.Prompter
	// decided reports whether anything was left to the user's judgement.
	decided bool
}

// resolveSearch turns a free-text query into a spec ready to be requested.
func resolveSearch(
	ctx context.Context,
	lookup mediaLookup,
	p *prompt.Prompter,
	opts searchOptions,
) (domain.MediaSpec, error) {
	r := &resolver{lookup: lookup, p: p}

	results, err := lookup.SearchMulti(ctx, opts.Query)
	if err != nil {
		return domain.MediaSpec{}, err
	}
	if len(results) == 0 {
		return domain.MediaSpec{}, fmt.Errorf("nothing on TMDB matches %q", opts.Query)
	}

	chosen, err := r.pickResult(results, opts.Pick)
	if err != nil {
		return domain.MediaSpec{}, err
	}

	spec, err := r.specFor(ctx, chosen.Ref, opts.Season)
	if err != nil {
		return domain.MediaSpec{}, err
	}

	if err := r.confirm(spec); err != nil {
		return domain.MediaSpec{}, err
	}
	return spec, nil
}

// resolveByID builds a spec for a title the caller named outright.
//
// This is the non-interactive path: `mediad add tv 1399 --season 2` has to work
// in a script, where there is nothing to ask. It does not confirm anything —
// the caller typed the identity of the thing they want, which is a clearer
// statement of intent than a menu selection.
func resolveByID(
	ctx context.Context,
	lookup mediaLookup,
	ref domain.MediaRef,
	p *prompt.Prompter,
	season *int,
) (domain.MediaSpec, error) {
	if err := ref.Valid(); err != nil {
		return domain.MediaSpec{}, err
	}

	r := &resolver{lookup: lookup, p: p, decided: true}
	return r.specFor(ctx, ref, season)
}

// specFor loads the metadata and resolves the season, if there is one.
func (r *resolver) specFor(ctx context.Context, ref domain.MediaRef, season *int) (domain.MediaSpec, error) {
	load := r.lookup.Movie
	if ref.Type == domain.MediaTypeTV {
		load = r.lookup.TV
	}

	title, err := load(ctx, ref.TMDBID)
	if err != nil {
		return domain.MediaSpec{}, err
	}
	// A client that reports no error and returns nothing is breaking its own
	// contract, and the alternative to saying so is a crash somewhere further
	// down where the reason is no longer visible.
	if title == nil {
		return domain.MediaSpec{}, fmt.Errorf("tmdb returned nothing for %s", ref)
	}

	if ref.Type == domain.MediaTypeMovie {
		return title.ToSpec(), nil
	}

	chosen, err := r.pickSeason(title, season)
	if err != nil {
		return domain.MediaSpec{}, err
	}

	return seasonSpec(title, chosen), nil
}

// seasonSpec freezes a series and one of its seasons into a request.
//
// The release date comes from the season, not from the series, and this is the
// part that is easy to get wrong. A user asking for season 5 of a series that
// began in 2008 is asking for episodes that may not exist yet; carrying the
// series' première date would tell the workflow that the media was released
// eighteen years ago, and it would search for a season that has not aired.
func seasonSpec(show *tmdb.Title, season tmdb.Season) domain.MediaSpec {
	spec := show.ToSpec()
	spec.Ref = domain.SeasonRef(show.Ref.TMDBID, season.Number)

	if season.AirDate != nil {
		spec.ReleasedAt = season.AirDate
	}
	return spec
}

// pickResult chooses among the search results.
func (r *resolver) pickResult(results []tmdb.SearchResult, pick int) (tmdb.SearchResult, error) {
	if pick > 0 {
		if pick > len(results) {
			return tmdb.SearchResult{}, fmt.Errorf(
				"--pick %d: there %s only %d", pick, plural(len(results), "is", "are"), len(results))
		}
		r.decided = true
		return results[pick-1], nil
	}

	if len(results) == 1 {
		// Unambiguous, so a script can proceed without a flag. Nobody has
		// decided anything yet, which is what the confirmation is for.
		return results[0], nil
	}

	if r.p == nil {
		return tmdb.SearchResult{}, fmt.Errorf(
			"%q matched %d titles and there is nobody to ask; "+
				"run it from a terminal or pass --pick", results[0].Title, len(results))
	}

	options := make([]string, 0, len(results))
	for _, res := range results {
		options = append(options, describeResult(res))
	}

	idx, err := r.p.Choose(fmt.Sprintf("%d titles match:", len(results)), options)
	if err != nil {
		return tmdb.SearchResult{}, err
	}
	r.decided = true
	return results[idx], nil
}

// pickSeason chooses a season of a series.
func (r *resolver) pickSeason(show *tmdb.Title, season *int) (tmdb.Season, error) {
	if len(show.Seasons) == 0 {
		return tmdb.Season{}, fmt.Errorf("%s has no seasons on TMDB", show.Title)
	}

	if season != nil {
		for _, s := range show.Seasons {
			if s.Number == *season {
				r.decided = true
				return s, nil
			}
		}
		return tmdb.Season{}, fmt.Errorf("%s has no season %d; it has %s",
			show.Title, *season, seasonList(show.Seasons))
	}

	if r.p == nil {
		return tmdb.Season{}, fmt.Errorf(
			"%s is a series and there is nobody to ask which season; pass --season (%s)",
			show.Title, seasonList(show.Seasons))
	}

	now := time.Now()
	options := make([]string, 0, len(show.Seasons))
	for _, s := range show.Seasons {
		options = append(options, describeSeason(s, now))
	}

	idx, err := r.p.Choose(fmt.Sprintf("%s — which season?", show.Title), options)
	if err != nil {
		return tmdb.Season{}, err
	}
	r.decided = true
	return show.Seasons[idx], nil
}

// confirm shows what is about to be requested and asks, when there is a person
// and they have not already said so.
func (r *resolver) confirm(spec domain.MediaSpec) error {
	if r.p == nil || r.decided {
		return nil
	}

	fmt.Fprintf(r.p.Out(), "will request: %s  [%s]\n", describeSpec(spec), spec.Ref.WorkflowID())

	ok, err := r.p.Confirm("start looking for it?", true)
	if err != nil {
		return err
	}
	if !ok {
		return prompt.ErrAborted
	}
	return nil
}

// describeResult renders one search hit for the menu.
func describeResult(r tmdb.SearchResult) string {
	kind := "film"
	if r.Ref.Type == domain.MediaTypeTV {
		kind = "series"
	}

	line := r.Title
	if r.Year > 0 {
		line += fmt.Sprintf(" (%d)", r.Year)
	}
	line += " — " + kind

	if r.OriginalTitle != "" && r.OriginalTitle != r.Title {
		line += ", " + r.OriginalTitle
	}
	return line + fmt.Sprintf("  [%s]", r.Ref.WorkflowID())
}

// describeSpec renders what is about to be requested.
func describeSpec(spec domain.MediaSpec) string {
	line := spec.DisplayTitle()
	if spec.Year > 0 {
		line += fmt.Sprintf(" (%d)", spec.Year)
	}
	if spec.Ref.IsSeason() {
		line += fmt.Sprintf(", сезон %d", spec.Ref.SeasonNumber())
	}
	return line
}

// describeSeason renders one season for the menu.
//
// A season that has not aired yet says so, because requesting one is
// reasonable — it will be found when it appears — but a person should know
// that is what they are doing rather than expect a download tonight.
func describeSeason(s tmdb.Season, now time.Time) string {
	line := fmt.Sprintf("сезон %d", s.Number)
	if s.Name != "" && !strings.EqualFold(s.Name, fmt.Sprintf("Season %d", s.Number)) {
		line += " — " + s.Name
	}

	if s.EpisodeCount > 0 {
		line += fmt.Sprintf(", %d %s", s.EpisodeCount, plural(s.EpisodeCount, "эпизод", "эпизодов"))
	}

	switch {
	case s.AirDate == nil:
		line += ", дата выхода неизвестна"
	case s.AirDate.After(now):
		line += ", начнётся " + s.AirDate.Format(time.DateOnly)
	default:
		line += ", " + s.AirDate.Format(time.DateOnly)
	}
	return line
}

// seasonList renders the available season numbers compactly.
func seasonList(seasons []tmdb.Season) string {
	numbers := make([]string, 0, len(seasons))
	for _, s := range seasons {
		numbers = append(numbers, strconv.Itoa(s.Number))
	}
	return "seasons " + strings.Join(numbers, ", ")
}

// plural picks a word ending, and the verb form that goes with it.
//
// Two forms rather than the three Russian needs, because these strings are
// short labels and dragging a pluralisation rules engine in for them would be
// more code than the labels.
func plural(n int, one, many string) string {
	if n%10 == 1 && n%100 != 11 {
		return one
	}
	return many
}
