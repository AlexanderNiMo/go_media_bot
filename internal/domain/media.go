// Package domain holds the types that cross package boundaries: the media
// identity every layer agrees on, the releases the indexer produces and the
// selector consumes, and the torrent state the download watcher reports.
//
// It deliberately has no dependencies. Anything that needs HTTP, a database or
// a clock lives in the package that owns that concern.
package domain

import (
	"fmt"
	"strconv"
	"time"
)

// MediaType distinguishes the two things that can be requested.
type MediaType string

const (
	MediaTypeMovie MediaType = "movie"
	MediaTypeTV    MediaType = "tv"
)

// Valid reports whether the value is one this project understands.
func (m MediaType) Valid() bool {
	return m == MediaTypeMovie || m == MediaTypeTV
}

// MediaRef identifies one unit of media: a film, or one season of a series.
//
// There is deliberately no "whole series" case. Requesting a series means
// requesting its seasons, each as its own request — a flat model that keeps the
// workflow, the deduplication key and the UI card all describing the same
// thing.
type MediaRef struct {
	Type   MediaType
	TMDBID int64
	// Season is the season number for a series and nil for a film.
	//
	// It is a pointer rather than an int with a sentinel because TMDB numbers
	// specials as season 0, so a plain zero value cannot double as "not
	// applicable" without silently meaning two different things.
	Season *int
}

// MovieRef returns a reference to a film.
func MovieRef(tmdbID int64) MediaRef {
	return MediaRef{Type: MediaTypeMovie, TMDBID: tmdbID}
}

// SeasonRef returns a reference to one season of a series.
func SeasonRef(tmdbID int64, season int) MediaRef {
	s := season
	return MediaRef{Type: MediaTypeTV, TMDBID: tmdbID, Season: &s}
}

// SeasonNumber returns the season, or 0 for a film. Callers that need to
// distinguish "season 0" from "no season" must check IsSeason first.
func (r MediaRef) SeasonNumber() int {
	if r.Season == nil {
		return 0
	}
	return *r.Season
}

// IsSeason reports whether this reference is to a specific season.
func (r MediaRef) IsSeason() bool { return r.Season != nil }

// WorkflowID is the Temporal workflow ID for this reference.
//
// It doubles as the idempotency key, so it has to be stable and readable in
// the Temporal UI: "movie-603", "tv-1399-s2". It carries no personal data,
// because workflow IDs are visible in the UI, the CLI and the event history.
func (r MediaRef) WorkflowID() string {
	if r.Season == nil {
		return fmt.Sprintf("%s-%d", r.Type, r.TMDBID)
	}
	return fmt.Sprintf("%s-%d-s%d", r.Type, r.TMDBID, *r.Season)
}

// String renders the reference the same way WorkflowID does.
func (r MediaRef) String() string { return r.WorkflowID() }

// Valid reports whether the reference is structurally sound.
//
// A series reference with no season passes: that is how a show is named in
// search results, before the user has picked a season. ValidForRequest is the
// stricter check, and it is the one that matters when starting a workflow.
func (r MediaRef) Valid() error {
	switch {
	case !r.Type.Valid():
		return fmt.Errorf("unknown media type %q", r.Type)
	case r.TMDBID <= 0:
		return fmt.Errorf("tmdb id must be positive, got %d", r.TMDBID)
	case r.Type == MediaTypeMovie && r.Season != nil:
		return fmt.Errorf("a film cannot have a season")
	case r.Season != nil && *r.Season < 0:
		return fmt.Errorf("season must not be negative, got %d", *r.Season)
	}
	return nil
}

// ValidForRequest reports whether this reference names something that can
// actually be downloaded.
//
// The extra requirement over Valid is that a series must name a season: this
// project has no "the whole series" request, by design, so a seasonless series
// reference is a UI state, not a job.
func (r MediaRef) ValidForRequest() error {
	if err := r.Valid(); err != nil {
		return err
	}
	if r.Type == MediaTypeTV && r.Season == nil {
		return fmt.Errorf("a series request must name a season")
	}
	return nil
}

// MediaSpec is everything the search and selection need to know about a title.
//
// It is captured once, when the request is created, and then travels inside
// the workflow input. Re-reading it from TMDB on every hourly search would
// make the loop depend on a service that can change under it, and would make
// an already-running workflow behave differently after a TMDB edit.
type MediaSpec struct {
	Ref           MediaRef
	Title         string
	TitleRU       string
	OriginalTitle string
	Year          int
	IMDBID        string
	TVDBID        int64
	RuntimeMin    int
	// PosterPath is TMDB's relative path, not a URL: the image host and the
	// size are presentation choices, and baking one into a value that travels
	// through a two-year workflow would fix it forever.
	PosterPath string
	// Overview is the synopsis, kept so that a request can be described
	// without asking TMDB again — which is the point of the spec.
	Overview string
	// ReleasedAt is nil when TMDB has no date, which is common for announced
	// titles. The search loop treats "unknown" as "not yet released" and backs
	// off rather than searching fruitlessly.
	ReleasedAt *time.Time
}

// SearchTerms returns the strings worth querying an indexer with, most
// specific first.
//
// The Russian title comes first: the trackers this project targets are
// Russian-language, and they name releases in Russian. The original title is
// the fallback for a release uploaded under its international name.
func (s MediaSpec) SearchTerms() []string {
	seen := make(map[string]bool, 3)
	var out []string

	for _, t := range []string{s.TitleRU, s.Title, s.OriginalTitle} {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// DisplayTitle is the name to show a user.
func (s MediaSpec) DisplayTitle() string {
	if s.TitleRU != "" {
		return s.TitleRU
	}
	return s.Title
}

// String renders the spec for logs.
func (s MediaSpec) String() string {
	year := ""
	if s.Year > 0 {
		year = " (" + strconv.Itoa(s.Year) + ")"
	}
	return s.DisplayTitle() + year + " [" + s.Ref.WorkflowID() + "]"
}
