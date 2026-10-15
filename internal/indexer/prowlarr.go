package indexer

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

	"golang.org/x/time/rate"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// maxProwlarrBytes bounds a response. An aggregate search across many trackers
// is the largest thing this client reads.
const maxProwlarrBytes = 32 << 20

// Prowlarr queries Prowlarr's own aggregate search API.
//
// It is used instead of Prowlarr's Torznab endpoint because that endpoint
// serves one indexer at a time — there is no "/all" equivalent to Jackett's —
// so covering five trackers would mean five requests against a service that
// already fans every one of them out internally. The JSON API does the fan-out
// server-side and returns JSON, which needs no XML parsing.
type Prowlarr struct {
	http       *http.Client
	baseURL    string
	apiKey     string
	indexerIDs []int
	log        *slog.Logger
	limiter    *rate.Limiter
}

// NewProwlarr builds a Prowlarr client.
func NewProwlarr(cfg *config.Config, log *slog.Logger) *Prowlarr {
	return &Prowlarr{
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
		baseURL:    strings.TrimRight(cfg.Indexer.BaseURL, "/"),
		apiKey:     cfg.Indexer.APIKey,
		indexerIDs: cfg.Indexer.IndexerIDs,
		log:        log.With("component", "indexer", "mode", "prowlarr"),
		limiter:    rate.NewLimiter(rate.Limit(cfg.Indexer.RatePerSecond), 1),
	}
}

// Name identifies the front end.
func (p *Prowlarr) Name() string { return "prowlarr" }

// Caps reports what Prowlarr can be asked for.
//
// It is not queried: Prowlarr translates a request into each indexer's own
// dialect itself and simply omits an identifier an indexer cannot use. So
// every identifier may be sent, and there is nothing to discover.
func (p *Prowlarr) Caps(context.Context) (Caps, error) {
	all := Capability{Available: true, Params: map[string]bool{
		"q": true, "imdbid": true, "tvdbid": true, "season": true, "ep": true,
	}}
	return Caps{Search: all, MovieQuery: all, TVQuery: all}, nil
}

// Search queries every configured indexer at once.
func (p *Prowlarr) Search(ctx context.Context, req SearchRequest) ([]domain.Release, error) {
	if p.baseURL == "" {
		return nil, errors.New("prowlarr base url is not configured")
	}
	if p.apiKey == "" {
		return nil, errors.New("prowlarr api key is not configured")
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("indexer rate limiter: %w", err)
	}

	params := url.Values{}
	// Prowlarr maps its `type` onto the Torznab `t` parameter internally, so
	// the same vocabulary applies.
	params.Set("type", searchType(req))
	params.Set("query", req.Query)
	params.Set("limit", strconv.Itoa(orDefault(req.Limit, 100)))
	params.Set("offset", "0")

	if req.IMDBID != "" {
		params.Set("imdbId", req.IMDBID)
	}
	if req.TVDBID > 0 {
		params.Set("tvdbId", strconv.FormatInt(req.TVDBID, 10))
	}
	if req.Season != nil {
		params.Set("season", strconv.Itoa(*req.Season))
	}
	if req.Episode != nil {
		params.Set("episode", strconv.Itoa(*req.Episode))
	}
	if len(req.Categories) > 0 {
		for _, c := range req.Categories {
			params.Add("categories", strconv.Itoa(c))
		}
	}
	// An empty list means every indexer, which is Prowlarr's own default.
	if len(p.indexerIDs) > 0 {
		for _, id := range p.indexerIDs {
			params.Add("indexerIds", strconv.Itoa(id))
		}
	}

	endpoint := p.baseURL + "/api/v1/search?" + params.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build prowlarr request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Api-Key", p.apiKey)

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call prowlarr: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProwlarrBytes))
	if err != nil {
		return nil, fmt.Errorf("read prowlarr response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &Error{StatusCode: resp.StatusCode, Body: truncate(string(body), 300)}
	}

	var raw []prowlarrRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode prowlarr response: %w", err)
	}

	// Prowlarr does not guarantee an ordering, and the selector's tie-breaking
	// should not depend on the order a service happened to return results in.
	releases := make([]domain.Release, 0, len(raw))
	for _, r := range raw {
		releases = append(releases, r.toRelease())
	}
	return releases, nil
}

func searchType(req SearchRequest) string {
	switch {
	case req.Season != nil:
		return "tvsearch"
	case req.IMDBID != "":
		return "movie"
	default:
		return "search"
	}
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

type prowlarrRelease struct {
	Title       string             `json:"title"`
	GUID        string             `json:"guid"`
	Size        int64              `json:"size"`
	Seeders     *int               `json:"seeders"`
	Leechers    *int               `json:"leechers"`
	DownloadURL string             `json:"downloadUrl"`
	MagnetURL   string             `json:"magnetUrl"`
	InfoHash    string             `json:"infoHash"`
	Indexer     string             `json:"indexer"`
	IndexerID   int                `json:"indexerId"`
	Categories  []prowlarrCategory `json:"categories"`
	PublishDate string             `json:"publishDate"`
}

func (r prowlarrRelease) toRelease() domain.Release {
	rel := domain.Release{
		GUID:        r.GUID,
		InfoHash:    strings.ToLower(r.InfoHash),
		Title:       strings.TrimSpace(r.Title),
		SizeBytes:   r.Size,
		Indexer:     r.Indexer,
		DownloadURL: r.DownloadURL,
		MagnetURI:   r.MagnetURL,
		Categories:  flattenProwlarrCategories(r.Categories),
	}

	// searchers/leechers are nullable in the API, and a null must not read as
	// "zero seeders" — that would make every release from an indexer that
	// omits them fail the minimum-seeders filter.
	if r.Seeders != nil {
		rel.Seeders = *r.Seeders
	} else {
		rel.Seeders = -1
	}
	if r.Leechers != nil {
		rel.Leechers = *r.Leechers
	}

	// Fall back to the indexer id as the origin label when the name is absent,
	// so a release is never unattributed.
	if rel.Indexer == "" && r.IndexerID != 0 {
		rel.Indexer = "indexer-" + strconv.Itoa(r.IndexerID)
	}

	rel.PublishedAt = parseProwlarrTime(r.PublishDate)

	return rel
}

// parseProwlarrTime handles the formats Prowlarr emits.
//
// It returns the zero time rather than an error: a release with no parseable
// date is still perfectly downloadable, and failing the whole search over it
// would be worse than losing one tie-breaker input.
func parseProwlarrTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// prowlarrCategory is one entry in Prowlarr's nested categories array.
//
// The Prowlarr API does not return a flat list of ids; it returns a tree
// shaped like
//
//	[{id: 7000, name: "Books", subCategories: [{id: 7010, ...}, ...]}, ...]
//
// The release selector works against a flat list of ids, so the tree is
// walked into one here. Duplicates are not deduplicated: a release that
// appears in two categories at the same level of the API response carries
// the id twice, and a download that matches the higher-level category must
// pass through the same filter.
type prowlarrCategory struct {
	ID            int                `json:"id"`
	SubCategories []prowlarrCategory `json:"subCategories"`
}

func flattenProwlarrCategories(cats []prowlarrCategory) []int {
	if len(cats) == 0 {
		return nil
	}
	out := make([]int, 0, len(cats)*2)
	var walk func([]prowlarrCategory)
	walk = func(cs []prowlarrCategory) {
		for _, c := range cs {
			out = append(out, c.ID)
			if len(c.SubCategories) > 0 {
				walk(c.SubCategories)
			}
		}
	}
	walk(cats)
	return out
}
