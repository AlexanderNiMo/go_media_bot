// Package plex is a client for the Plex Media Server HTTP API.
//
// It is written against net/http, about 150 lines of it, because the one
// usable Go library has no semver tags — so it can only be pinned to a
// pseudo-version that never updates — and its search method calls a legacy
// endpoint. Three endpoints are all this project needs.
//
// Two things about the Plex API are worth knowing before reading the code:
//
//   - PMS answers with XML unless asked for JSON. Every request sends
//     Accept: application/json.
//
//   - GET /library/search is NOT a local endpoint. It is Plex Discover, a
//     cloud service, and it knows nothing about your server's library.
//     Existence checks go through the section listing instead, matched on the
//     tmdb:// guid.
package plex

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
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// Module wires the Plex client into the fx graph.
var Module = fx.Module("plex", fx.Provide(New))

const (
	// maxResponseBytes bounds a response body. A section listing for a large
	// library is a few megabytes.
	maxResponseBytes = 32 << 20

	// productName and productVersion identify this client to the server.
	// Plex expects them and logs them; omitting them makes requests look like
	// they come from nowhere.
	productName    = "mediad"
	productVersion = "0.1.0"
)

// Client is a Plex Media Server client. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
	log     *slog.Logger

	// sections caches the library list. It changes only when the user edits
	// their libraries, so it is refreshed on a TTL rather than per check.
	mu          sync.Mutex
	sections    []Section
	sectionsAt  time.Time
	sectionsTTL time.Duration
}

// New builds a client from the configuration.
func New(cfg *config.Config, log *slog.Logger) *Client {
	return &Client{
		http: &http.Client{
			// A backstop; the caller's context carries the real deadline.
			Timeout: 60 * time.Second,
		},
		baseURL:     strings.TrimRight(cfg.Plex.URL, "/"),
		token:       cfg.Plex.Token,
		log:         log.With("component", "plex"),
		sectionsTTL: cfg.Plex.SectionsTTL,
	}
}

// Configured reports whether a server URL and token are present.
func (c *Client) Configured() bool {
	return c.baseURL != "" && c.token != ""
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Section is one library on the server.
type Section struct {
	Key   string
	Title string
	// Type is Plex's section type: "movie" or "show".
	Type string
}

// MovieSection reports whether this section holds films.
func (s Section) MovieSection() bool { return s.Type == "movie" }

// ShowSection reports whether this section holds series.
func (s Section) ShowSection() bool { return s.Type == "show" }

// Match describes something found in the library.
type Match struct {
	RatingKey  string
	SectionKey string
	Title      string
	Year       int
	// GUIDs are the external identifiers Plex recorded, such as
	// "tmdb://603" or "imdb://tt0133093".
	GUIDs   []string
	AddedAt time.Time
	// LeafCount is the number of episodes, for a season.
	LeafCount int
}

// TMDBID extracts the TMDB id from the recorded GUIDs.
//
// Matching is done on this and never on the title: titles are localisable,
// ambiguous and edited by hand, so a title match produces both false positives
// (a remake) and false negatives (a renamed file), and both are invisible to
// the user.
func (m Match) TMDBID() (int64, bool) {
	for _, g := range m.GUIDs {
		if rest, ok := strings.CutPrefix(g, "tmdb://"); ok {
			id, err := strconv.ParseInt(rest, 10, 64)
			if err == nil {
				return id, true
			}
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// API calls
// ---------------------------------------------------------------------------

// Sections returns the library list, served from a short-lived cache.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	c.mu.Lock()
	if time.Since(c.sectionsAt) < c.sectionsTTL && c.sections != nil {
		cached := c.sections
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	var raw struct {
		MediaContainer struct {
			Directory []struct {
				Key   string `json:"key"`
				Title string `json:"title"`
				Type  string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}

	if err := c.get(ctx, "/library/sections", nil, &raw); err != nil {
		return nil, err
	}

	sections := make([]Section, 0, len(raw.MediaContainer.Directory))
	for _, d := range raw.MediaContainer.Directory {
		sections = append(sections, Section{Key: d.Key, Title: d.Title, Type: d.Type})
	}

	c.mu.Lock()
	c.sections = sections
	c.sectionsAt = time.Now()
	c.mu.Unlock()

	return sections, nil
}

// FindMovie looks for a film by TMDB id.
//
// Kept for tests and the old guid-matching path. The application itself uses
// FindMovieByTitle, because the live library often has no tmdb:// guid on
// individual items.
func (c *Client) FindMovie(ctx context.Context, ref domain.MediaRef, title string) (*Match, error) {
	if ref.Type != domain.MediaTypeMovie {
		return nil, fmt.Errorf("FindMovie called with %s", ref.Type)
	}
	return c.find(ctx, "movie", ref, title, nil)
}

// FindSeason looks for one season of a series by TMDB id.
//
// Kept for tests; the application uses FindSeasonByTitle, for the same
// reason as FindMovie.
func (c *Client) FindSeason(ctx context.Context, ref domain.MediaRef, title string) (*Match, error) {
	if ref.Type != domain.MediaTypeTV || ref.Season == nil {
		return nil, fmt.Errorf("FindSeason needs a series reference with a season")
	}
	return c.find(ctx, "show", ref, title, ref.Season)
}

// FindMovieByTitle looks for a film by title and year.
//
// Plex items carry an internal "plex://" id and, on items the user tagged,
// a tmdb:// or imdb:// guid. Real libraries have a long tail of items with no
// external guid, and those are the ones this method exists to find.
//
// A match is a normalised title equal to one of the candidates AND the year
// equal to want. The candidate list is the spec's TitleRU, Title and
// OriginalTitle in that order; the first that matches wins. The Plex-side
// ?title= prefilter uses the same first candidate, so the server returns a
// small list; the in-Go comparison finishes the work.
func (c *Client) FindMovieByTitle(ctx context.Context, candidates []string, year int) (*Match, error) {
	return c.findByTitle(ctx, "movie", candidates, year, nil)
}

// FindSeasonByTitle is FindMovieByTitle for series: the show is matched by
// title and year, and the season is then located among the show's children
// by its index. Plex records season numbers on the children but no external
// guid, so a year-anchored match on the show is the most we can do.
func (c *Client) FindSeasonByTitle(ctx context.Context, candidates []string, year int, season int) (*Match, error) {
	return c.findByTitle(ctx, "show", candidates, year, &season)
}

func (c *Client) find(ctx context.Context, sectionType string, ref domain.MediaRef, title string, season *int) (*Match, error) {
	if !c.Configured() {
		return nil, errors.New("plex is not configured")
	}

	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}

	for _, section := range sections {
		if section.Type != sectionType {
			continue
		}

		match, err := c.findInSection(ctx, section, ref, title, season)
		if err != nil {
			// One broken section must not fail the whole check: a library
			// being scanned returns errors that have nothing to do with the
			// title being looked for.
			c.log.Warn("plex section lookup failed",
				"section", section.Key, "title", section.Title, "error", err)
			continue
		}
		if match != nil {
			return match, nil
		}
	}

	return nil, nil
}

func (c *Client) findInSection(ctx context.Context, section Section, ref domain.MediaRef, title string, season *int) (*Match, error) {
	params := url.Values{
		// includeGuids is what makes guid matching possible at all; without it
		// the response carries no external identifiers.
		"includeGuids": {"1"},
		"type":         {plexTypeFor(section.Type)},
	}
	if title != "" {
		params.Set("title", title)
	}

	var raw mediaContainer
	if err := c.get(ctx, "/library/sections/"+section.Key+"/all", params, &raw); err != nil {
		return nil, err
	}

	for _, item := range raw.MediaContainer.Metadata {
		gotID, ok := item.tmdbID()
		if !ok || gotID != ref.TMDBID {
			continue
		}

		if season == nil {
			return item.toMatch(section.Key), nil
		}

		return c.findSeasonChild(ctx, section.Key, item, *season)
	}

	return nil, nil
}

// findSeasonChild locates one season among a show's children.
//
// Plex has no "season N of this show" endpoint, so the children are listed and
// matched on index. The returned Match carries the season's leaf count, which
// is what the post-download verification compares against.
func (c *Client) findSeasonChild(ctx context.Context, sectionKey string, show mediaItem, season int) (*Match, error) {
	if show.RatingKey == "" {
		return nil, nil
	}

	var raw mediaContainer
	if err := c.get(ctx, "/library/metadata/"+show.RatingKey+"/children", nil, &raw); err != nil {
		return nil, err
	}

	for _, child := range raw.MediaContainer.Metadata {
		if child.Index == nil || *child.Index != season {
			continue
		}
		match := child.toMatch(sectionKey)
		match.LeafCount = child.LeafCount
		match.Title = show.Title
		match.Year = show.Year
		// The show's guids are what identified it, and the caller matched on
		// them, so they are carried through for the record.
		match.GUIDs = show.GUIDs()
		return match, nil
	}

	return nil, nil
}

// EpisodeCount returns how many episodes of a season the server has.
//
// Used both to snapshot the count before a download and to check it grew
// afterwards. Returns 0 with no error when the show or season is absent, which
// is a normal state rather than a failure.
func (c *Client) EpisodeCount(ctx context.Context, ref domain.MediaRef, title string) (int, error) {
	if ref.Type != domain.MediaTypeTV || ref.Season == nil {
		return 0, errors.New("EpisodeCount needs a series reference with a season")
	}

	match, err := c.FindSeason(ctx, ref, title)
	if err != nil {
		return 0, err
	}
	if match == nil {
		return 0, nil
	}
	return match.LeafCount, nil
}

// RefreshSection asks the server to rescan one section.
//
// path narrows the scan to a single directory, which is what makes this cheap
// enough to run as part of every completed download. Without it Plex rescans
// the whole library, which on a large one takes minutes and hammers the disk.
func (c *Client) RefreshSection(ctx context.Context, sectionKey string, path string) error {
	params := url.Values{"force": {"0"}}
	if path != "" {
		params.Set("path", path)
	}

	var raw struct {
		MediaContainer struct {
			Size int `json:"size"`
		} `json:"MediaContainer"`
	}
	return c.get(ctx, "/library/sections/"+sectionKey+"/refresh", params, &raw)
}

// FindSectionFor returns the section a title should live in.
//
// Used to pick a section to refresh: the one that holds the library the
// download was filed under, by type.
func (c *Client) FindSectionFor(ctx context.Context, mediaType domain.MediaType) (*Section, error) {
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}

	want := "movie"
	if mediaType == domain.MediaTypeTV {
		want = "show"
	}

	for i := range sections {
		if sections[i].Type == want {
			return &sections[i], nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	endpoint := c.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build plex request: %w", err)
	}
	// Without this the server answers with XML, which unmarshals into an empty
	// struct rather than failing — a silent wrong answer instead of an error.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("X-Plex-Product", productName)
	req.Header.Set("X-Plex-Version", productVersion)
	req.Header.Set("X-Plex-Client-Identifier", productName)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call plex %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read plex response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &Error{StatusCode: resp.StatusCode, Path: path, Body: truncate(string(body), 300)}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode plex response for %s: %w", path, err)
	}
	return nil
}

// Error is a non-200 response from Plex.
type Error struct {
	StatusCode int
	Path       string
	Body       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("plex %s returned %d: %s", e.Path, e.StatusCode, e.Body)
}

// Unauthorized reports whether the token was rejected.
func (e *Error) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized
}

func plexTypeFor(sectionType string) string {
	// Plex's numeric types: 1 = movie, 2 = show.
	if sectionType == "movie" {
		return "1"
	}
	return "2"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

type mediaContainer struct {
	MediaContainer struct {
		Size     int         `json:"size"`
		Metadata []mediaItem `json:"Metadata"`
	} `json:"MediaContainer"`
}

type mediaItem struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	// Index is the season number on a season item, and the episode number on
	// an episode.
	Index     *int  `json:"index"`
	LeafCount int   `json:"leafCount"`
	AddedAt   int64 `json:"addedAt"`

	// Guid is a single entry and Guid is a list; which one appears depends on
	// the Plex version. Both are read.
	//
	// Note the case: Plex ships two fields. The lowercase 'guid' is a string
	// of the form "plex://movie/<ratingKey>" — the item's own URL inside
	// Plex. The capitalised 'Guid' is a list of objects with the external
	// ids (tmdb://, imdb://, tvdb://). Conflating them, or assuming the
	// lowercase form is a list, fails to decode every response.
	GUID []struct {
		ID string `json:"id"`
	} `json:"Guid"`
	//nolint:revive // Plex's own field name.
	Guid string `json:"guid"`
}

func (m mediaItem) GUIDs() []string {
	out := make([]string, 0, len(m.GUID)+1)
	for _, g := range m.GUID {
		if g.ID != "" {
			out = append(out, g.ID)
		}
	}
	if m.Guid != "" {
		out = append(out, m.Guid)
	}
	return out
}

func (m mediaItem) tmdbID() (int64, bool) {
	return Match{GUIDs: m.GUIDs()}.TMDBID()
}

func (m mediaItem) toMatch(sectionKey string) *Match {
	match := &Match{
		RatingKey:  m.RatingKey,
		SectionKey: sectionKey,
		Title:      m.Title,
		Year:       m.Year,
		GUIDs:      m.GUIDs(),
		LeafCount:  m.LeafCount,
	}
	if m.AddedAt > 0 {
		match.AddedAt = time.Unix(m.AddedAt, 0)
	}
	return match
}
