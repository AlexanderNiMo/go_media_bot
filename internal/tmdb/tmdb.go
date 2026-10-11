// Package tmdb is a client for the TMDB v3 API.
//
// It is written against net/http rather than a library because every method
// has to take a context.Context: these calls happen inside Temporal
// activities, and an activity that cannot be cancelled when its workflow is
// cancelled holds a worker slot until its timeout. The available Go libraries
// expose no context-aware methods.
//
// Only the four endpoints the project actually uses are implemented.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/fx"
	"golang.org/x/time/rate"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// Module wires the TMDB client into the fx graph.
var Module = fx.Module("tmdb", fx.Provide(New))

const (
	defaultBaseURL = "https://api.themoviedb.org/3"
	imageBaseURL   = "https://image.tmdb.org/t/p"

	// maxResponseBytes bounds a response body. TMDB responses are tens of
	// kilobytes; the limit exists so a misdirected request cannot exhaust
	// memory.
	maxResponseBytes = 4 << 20
)

// Client is a TMDB API client. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	baseURL string
	// Exactly one of apiKey and bearer is set. Both are accepted because both
	// remain supported; the bearer token is the documented default.
	apiKey   string
	bearer   string
	language string
	limiter  *rate.Limiter
	log      *slog.Logger
}

// New builds a client from the configuration.
func New(cfg *config.Config, log *slog.Logger) (*Client, error) {
	key := strings.TrimSpace(cfg.TMDB.APIKey)
	if key == "" {
		// Not fatal: the project can run without TMDB configured, and failing
		// at startup would make `mediad config` and `mediad migrate` unusable
		// before the API key has been obtained. Calls fail with a clear error
		// instead.
		log.Warn("tmdb.api_key is not set; searching by title will fail")
	}

	c := &Client{
		http: &http.Client{
			// A backstop only. The meaningful deadline comes from the caller's
			// context, which carries the activity's timeout.
			Timeout: 30 * time.Second,
		},
		baseURL:  defaultBaseURL,
		language: cfg.TMDB.Language,
		log:      log.With("component", "tmdb"),
		// TMDB dropped its published rate limit, but still answers 429 under
		// load. Ten per second sits well inside what it tolerates while a
		// human-driven search never approaches it.
		limiter: rate.NewLimiter(rate.Limit(10), 20),
	}

	// A v4 read access token is a JWT and contains dots; a v3 key does not.
	// Guessing between them matters because using the wrong one produces a 401
	// that says nothing about which of the two was expected.
	if strings.Count(key, ".") == 2 {
		c.bearer = key
	} else {
		c.apiKey = key
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// SearchResult is one hit from a multi-search.
type SearchResult struct {
	Ref           domain.MediaRef
	Title         string
	OriginalTitle string
	Overview      string
	PosterPath    string
	Year          int
	VoteAverage   float64
	Popularity    float64
}

// Title is a film or series as TMDB describes it.
type Title struct {
	Ref           domain.MediaRef
	Title         string
	OriginalTitle string
	Overview      string
	PosterPath    string
	Year          int
	RuntimeMin    int
	ReleasedAt    *time.Time
	IMDBID        string
	TVDBID        int64

	// Seasons is populated for series only; each entry carries the episode
	// count and air date the season picker shows.
	Seasons []Season
}

// Season is one season of a series, as listed on the series itself.
type Season struct {
	Number       int
	Name         string
	Overview     string
	EpisodeCount int
	AirDate      *time.Time
	PosterPath   string
}

// SeasonDetails is one season with its episodes.
type SeasonDetails struct {
	Number       int
	Name         string
	Overview     string
	AirDate      *time.Time
	PosterPath   string
	EpisodeCount int
	Episodes     []Episode
}

// Episode is one episode of a season.
type Episode struct {
	Number      int
	Name        string
	Overview    string
	AirDate     *time.Time
	RuntimeMin  int
	EpisodeType string
}

// AiredCount returns how many episodes have aired by now.
//
// This is what tells a season request whether there is anything to look for
// yet: a season that has not started airing has no releases, and searching for
// it hourly would be pure waste.
func (s SeasonDetails) AiredCount(now time.Time) int {
	n := 0
	for _, e := range s.Episodes {
		if e.AirDate != nil && !e.AirDate.After(now) {
			n++
		}
	}
	return n
}

// NewestAirDate returns the latest air date among the episodes, or nil.
func (s SeasonDetails) NewestAirDate() *time.Time {
	var newest *time.Time
	for _, e := range s.Episodes {
		if e.AirDate == nil {
			continue
		}
		if newest == nil || e.AirDate.After(*newest) {
			newest = e.AirDate
		}
	}
	return newest
}

// ToSpec converts a title into the frozen spec a workflow carries.
//
// The Russian title is only recorded when TMDB actually localised it. When no
// translation exists the API returns the original string with no flag saying
// so, and treating that as a Russian title would make the release selector
// prefer Russian releases for a title the user never asked for in Russian —
// and, worse, would put an English string in title_ru where the UI promises
// Russian.
func (t Title) ToSpec() domain.MediaSpec {
	spec := domain.MediaSpec{
		Ref:           t.Ref,
		Title:         t.Title,
		OriginalTitle: t.OriginalTitle,
		Year:          t.Year,
		RuntimeMin:    t.RuntimeMin,
		ReleasedAt:    t.ReleasedAt,
		IMDBID:        t.IMDBID,
		TVDBID:        t.TVDBID,
	}
	if t.Title != "" && t.Title != t.OriginalTitle {
		spec.TitleRU = t.Title
	}
	return spec
}

// PosterURL builds a full image URL, or returns "" when there is no poster.
//
// poster_path is nullable, and it is genuinely absent for obscure titles, so
// callers have to handle the empty case rather than assume an image exists.
func PosterURL(path string, size string) string {
	if path == "" {
		return ""
	}
	if size == "" {
		size = "w500"
	}
	return imageBaseURL + "/" + size + path
}

// ---------------------------------------------------------------------------
// API calls
// ---------------------------------------------------------------------------

// SearchMulti searches films and series by name.
//
// People are filtered out: multi-search returns them, they have no poster and
// no release date, and offering an actor as a candidate for download is
// nonsense.
func (c *Client) SearchMulti(ctx context.Context, query string) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search query is empty")
	}

	var raw rawSearchMulti
	err := c.get(ctx, "/search/multi", url.Values{
		"query":         {query},
		"include_adult": {"false"},
		"page":          {"1"},
	}, &raw)
	if err != nil {
		return nil, err
	}

	out := make([]SearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		ref, ok := r.ref()
		if !ok {
			continue
		}
		out = append(out, SearchResult{
			Ref:           ref,
			Title:         r.displayTitle(),
			OriginalTitle: r.originalTitle(),
			Overview:      r.Overview,
			PosterPath:    r.PosterPath,
			Year:          yearOf(r.date()),
			VoteAverage:   r.VoteAverage,
			Popularity:    r.Popularity,
		})
	}
	return out, nil
}

// Movie returns a film's details.
func (c *Client) Movie(ctx context.Context, tmdbID int64) (*Title, error) {
	var raw rawMovie
	if err := c.get(ctx, "/movie/"+strconv.FormatInt(tmdbID, 10), nil, &raw); err != nil {
		return nil, err
	}

	return &Title{
		Ref:           domain.MovieRef(raw.ID),
		Title:         raw.Title,
		OriginalTitle: raw.OriginalTitle,
		Overview:      raw.Overview,
		PosterPath:    raw.PosterPath,
		Year:          yearOf(raw.ReleaseDate),
		RuntimeMin:    raw.Runtime,
		ReleasedAt:    dateOf(raw.ReleaseDate),
		IMDBID:        raw.IMDBID,
	}, nil
}

// TV returns a series' details, including its season list.
func (c *Client) TV(ctx context.Context, tmdbID int64) (*Title, error) {
	var raw rawTV
	if err := c.get(ctx, "/tv/"+strconv.FormatInt(tmdbID, 10), nil, &raw); err != nil {
		return nil, err
	}

	seasons := make([]Season, 0, len(raw.Seasons))
	for _, s := range raw.Seasons {
		seasons = append(seasons, Season{
			Number:       s.SeasonNumber,
			Name:         s.Name,
			Overview:     s.Overview,
			EpisodeCount: s.EpisodeCount,
			AirDate:      dateOf(s.AirDate),
			PosterPath:   s.PosterPath,
		})
	}

	var runtime int
	if len(raw.EpisodeRunTime) > 0 {
		runtime = raw.EpisodeRunTime[0]
	}

	return &Title{
		Ref:           domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: raw.ID},
		Title:         raw.Name,
		OriginalTitle: raw.OriginalName,
		Overview:      raw.Overview,
		PosterPath:    raw.PosterPath,
		Year:          yearOf(raw.FirstAirDate),
		RuntimeMin:    runtime,
		ReleasedAt:    dateOf(raw.FirstAirDate),
		Seasons:       seasons,
	}, nil
}

// Season returns one season with its episodes.
//
// The episode list is what a season request is really about: it gives the air
// dates that decide whether searching is worthwhile yet, and the episode count
// that the Plex verification compares against.
func (c *Client) Season(ctx context.Context, tmdbID int64, season int) (*SeasonDetails, error) {
	path := "/tv/" + strconv.FormatInt(tmdbID, 10) + "/season/" + strconv.Itoa(season)

	var raw rawSeason
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}

	episodes := make([]Episode, 0, len(raw.Episodes))
	for _, e := range raw.Episodes {
		episodes = append(episodes, Episode{
			Number:      e.EpisodeNumber,
			Name:        e.Name,
			Overview:    e.Overview,
			AirDate:     dateOf(e.AirDate),
			RuntimeMin:  e.Runtime,
			EpisodeType: e.EpisodeType,
		})
	}

	return &SeasonDetails{
		Number:       raw.SeasonNumber,
		Name:         raw.Name,
		Overview:     raw.Overview,
		AirDate:      dateOf(raw.AirDate),
		PosterPath:   raw.PosterPath,
		EpisodeCount: len(episodes),
		Episodes:     episodes,
	}, nil
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	if c.apiKey == "" && c.bearer == "" {
		return errors.New("tmdb api key is not configured")
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("tmdb rate limiter: %w", err)
	}

	if params == nil {
		params = url.Values{}
	}
	if c.language != "" {
		params.Set("language", c.language)
	}
	// An api_key in the query is the v3 style; a bearer token goes in a header
	// instead, and sending both is not accepted.
	if c.apiKey != "" {
		params.Set("api_key", c.apiKey)
	}

	endpoint := c.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build tmdb request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call tmdb %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read tmdb response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &Error{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       truncate(string(body), 300),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode tmdb response for %s: %w", path, err)
	}
	return nil
}

// Error is a non-200 response from TMDB.
type Error struct {
	StatusCode int
	Path       string
	Body       string
	// RetryAfter is how long TMDB asked us to wait, when it said so.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("tmdb %s returned %d: %s", e.Path, e.StatusCode, e.Body)
}

// NotFound reports whether the title does not exist.
func (e *Error) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// RateLimited reports whether TMDB asked us to slow down.
func (e *Error) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// Unauthorized reports whether the API key was rejected.
func (e *Error) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Wire format
//
// These mirror the API exactly, including its habit of naming the same concept
// differently on films and series (title versus name, release_date versus
// first_air_date, and so on). The asymmetry is confined to this section; the
// mapping to domain types happens above.
// ---------------------------------------------------------------------------

type rawSearchMulti struct {
	Results []rawSearchItem `json:"results"`
}

type rawSearchItem struct {
	MediaType     string  `json:"media_type"`
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	VoteAverage   float64 `json:"vote_average"`
	Popularity    float64 `json:"popularity"`
}

// ref converts a search hit into a media reference.
//
// The second return value is false for anything that is not a film or a
// series — in practice a person, which multi-search returns alongside titles.
func (r rawSearchItem) ref() (domain.MediaRef, bool) {
	switch r.MediaType {
	case "movie":
		return domain.MovieRef(r.ID), true
	case "tv":
		// No season: a search hit names a show, and which season to fetch is
		// the next question the user answers.
		return domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: r.ID}, true
	default:
		return domain.MediaRef{}, false
	}
}

func (r rawSearchItem) displayTitle() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

func (r rawSearchItem) originalTitle() string {
	if r.OriginalTitle != "" {
		return r.OriginalTitle
	}
	return r.OriginalName
}

func (r rawSearchItem) date() string {
	if r.ReleaseDate != "" {
		return r.ReleaseDate
	}
	return r.FirstAirDate
}

type rawMovie struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	Overview      string `json:"overview"`
	PosterPath    string `json:"poster_path"`
	ReleaseDate   string `json:"release_date"`
	Runtime       int    `json:"runtime"`
	IMDBID        string `json:"imdb_id"`
}

type rawTV struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	OriginalName   string `json:"original_name"`
	Overview       string `json:"overview"`
	PosterPath     string `json:"poster_path"`
	FirstAirDate   string `json:"first_air_date"`
	EpisodeRunTime []int  `json:"episode_run_time"`
	Seasons        []struct {
		SeasonNumber int    `json:"season_number"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		EpisodeCount int    `json:"episode_count"`
		AirDate      string `json:"air_date"`
		PosterPath   string `json:"poster_path"`
	} `json:"seasons"`
}

type rawSeason struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	Overview     string `json:"overview"`
	AirDate      string `json:"air_date"`
	PosterPath   string `json:"poster_path"`
	Episodes     []struct {
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		Overview      string `json:"overview"`
		AirDate       string `json:"air_date"`
		Runtime       int    `json:"runtime"`
		EpisodeType   string `json:"episode_type"`
	} `json:"episodes"`
}

const dateLayout = "2006-01-02"

// dateOf parses an API date, which is "YYYY-MM-DD" or an empty string.
//
// TMDB returns "" rather than null for a title with no date — an announced
// film, or an unaired season — so an empty string is the normal "unknown"
// case and must not be an error.
func dateOf(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return nil
	}
	return &t
}

func yearOf(s string) int {
	if t := dateOf(s); t != nil {
		return t.Year()
	}
	return 0
}
