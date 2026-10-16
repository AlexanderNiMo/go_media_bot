package indexer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// torznabNS is the Torznab attribute namespace.
//
// Attributes are namespaced, so the XML decoder has to be told the full
// namespace rather than just the element name — `xml:"attr"` would match
// nothing and every release would silently arrive with no seeders, no size and
// no infohash.
const torznabNS = "http://torznab.com/schemas/2015/feed"

// maxTorznabBytes bounds a response. A hundred-result feed with extended
// attributes runs to a few hundred kilobytes.
const maxTorznabBytes = 16 << 20

// Torznab is a client for a Torznab endpoint.
//
// With Jackett the base URL is the aggregate endpoint
// (/api/v2.0/indexers/all/results/torznab), which searches every configured
// tracker at once. With Prowlarr there is no aggregate endpoint — the base URL
// must point at one indexer (/<id>), and each indexer needs its own client.
type Torznab struct {
	http    *http.Client
	baseURL string
	apiKey  string
	name    string
	log     *slog.Logger

	// threadPatterns is the per-tracker regex set for the forum thread
	// id. Compile once, apply to every release's description.
	threadPatterns  []config.ThreadPattern
	fallbackPattern *regexp.Regexp

	// limiter is a second line of defence.
	//
	// The primary rate limit lives on the Temporal task queue, which is
	// server-enforced and shared across every worker replica. This one exists
	// because the same client is used from the CLI, outside any worker, and
	// because a client that can be pointed at a tracker should not be able to
	// emit an unbounded burst even if the queue configuration is wrong.
	limiter *rate.Limiter

	mu      sync.Mutex
	caps    Caps
	capsAt  time.Time
	capsTTL time.Duration
}

// NewTorznab builds a Torznab client.
func NewTorznab(cfg *config.Config, log *slog.Logger) *Torznab {
	return &Torznab{
		http: &http.Client{
			Timeout: 90 * time.Second,
		},
		baseURL:         strings.TrimRight(cfg.Indexer.BaseURL, "/"),
		apiKey:          cfg.Indexer.APIKey,
		name:            "torznab",
		log:             log.With("component", "indexer", "mode", "torznab"),
		limiter:         rate.NewLimiter(rate.Limit(cfg.Indexer.RatePerSecond), 1),
		capsTTL:         cfg.Indexer.CapsTTL,
		threadPatterns:  cfg.Indexer.ThreadPatterns,
		fallbackPattern: defaultThreadFallback(),
	}
}

// Name identifies the front end.
func (t *Torznab) Name() string { return t.name }

// Search queries the indexer.
func (t *Torznab) Search(ctx context.Context, req SearchRequest) ([]domain.Release, error) {
	caps, err := t.Caps(ctx)
	if err != nil {
		// Capabilities are an optimisation, not a prerequisite. Without them
		// the free-text query still works, so this degrades instead of
		// failing.
		t.log.Warn("torznab caps unavailable, falling back to a text query", "error", err)
		caps = Caps{}
	}

	params := url.Values{}
	switch {
	case req.Season != nil && caps.TVQuery.Supports("season"):
		params.Set("t", "tvsearch")
		params.Set("season", strconv.Itoa(*req.Season))
		if req.TVDBID > 0 && caps.TVQuery.Supports("tvdbid") {
			params.Set("tvdbid", strconv.FormatInt(req.TVDBID, 10))
		}
		if req.Episode != nil && caps.TVQuery.Supports("ep") {
			params.Set("ep", strconv.Itoa(*req.Episode))
		}
		// A query without a usable id still needs the text, because most
		// trackers match on the release name.
		params.Set("q", req.Query)

	case req.IMDBID != "" && caps.MovieQuery.Supports("imdbid"):
		params.Set("t", "movie")
		params.Set("imdbid", req.IMDBID)
		params.Set("q", req.Query)

	default:
		params.Set("t", "search")
		params.Set("q", req.Query)
	}

	if len(req.Categories) > 0 {
		params.Set("cat", joinInts(req.Categories))
	}
	if req.Limit > 0 {
		params.Set("limit", strconv.Itoa(req.Limit))
	}
	// Ask for the attribute set explicitly; some front ends omit it otherwise.
	params.Set("extended", "1")

	var feed rssFeed
	if err := t.get(ctx, params, &feed); err != nil {
		return nil, err
	}

	threadPatterns := compileThreadPatterns(t.threadPatterns)

	releases := make([]domain.Release, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		rel := item.toRelease(t.name)
		rel.Comment, rel.ThreadID = extractThreadID(item.Description, threadPatterns, t.fallbackPattern)
		releases = append(releases, rel)
	}
	return releases, nil
}

// Caps returns the indexer's capabilities, cached on a TTL.
func (t *Torznab) Caps(ctx context.Context) (Caps, error) {
	t.mu.Lock()
	if !t.capsAt.IsZero() && time.Since(t.capsAt) < t.capsTTL {
		cached := t.caps
		t.mu.Unlock()
		return cached, nil
	}
	t.mu.Unlock()

	var raw struct {
		Searching struct {
			Search struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"search"`
			MovieSearch struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"movie-search"`
			TVSearch struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"tv-search"`
		} `xml:"searching"`
	}

	if err := t.get(ctx, url.Values{"t": {"caps"}}, &raw); err != nil {
		return Caps{}, err
	}

	caps := Caps{
		Search:     capability(raw.Searching.Search.Available, raw.Searching.Search.SupportedParams),
		MovieQuery: capability(raw.Searching.MovieSearch.Available, raw.Searching.MovieSearch.SupportedParams),
		TVQuery:    capability(raw.Searching.TVSearch.Available, raw.Searching.TVSearch.SupportedParams),
	}

	t.mu.Lock()
	t.caps = caps
	t.capsAt = time.Now()
	t.mu.Unlock()

	return caps, nil
}

func capability(available, params string) Capability {
	c := Capability{
		Available: available == "yes" || available == "true" || available == "1",
		Params:    map[string]bool{},
	}
	for _, p := range strings.Split(params, ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.Params[p] = true
		}
	}
	return c
}

func (t *Torznab) get(ctx context.Context, params url.Values, out any) error {
	if t.baseURL == "" {
		return errors.New("torznab base url is not configured")
	}
	if t.apiKey == "" {
		return errors.New("torznab api key is not configured")
	}

	if err := t.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("indexer rate limiter: %w", err)
	}

	params.Set("apikey", t.apiKey)

	endpoint := t.baseURL + "/api?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build torznab request: %w", err)
	}
	req.Header.Set("Accept", "application/xml, text/xml")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("call torznab: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorznabBytes))
	if err != nil {
		return fmt.Errorf("read torznab response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return &Error{StatusCode: resp.StatusCode, Body: truncate(string(body), 300)}
	}

	// A Torznab failure is a 200 with an <error> element, so the status code
	// alone is not enough to tell success from failure.
	var failure struct {
		XMLName xml.Name `xml:"error"`
		Code    int      `xml:"code,attr"`
		Message string   `xml:"description,attr"`
	}
	if err := xml.Unmarshal(body, &failure); err == nil && failure.XMLName.Local == "error" {
		return &Error{Code: failure.Code, Body: failure.Message}
	}

	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode torznab response: %w", err)
	}
	return nil
}

// Error is a failure reported by the indexer front end.
type Error struct {
	// StatusCode is set for an HTTP-level failure.
	StatusCode int
	// Code is the indexer's own error code, when it reported one.
	Code int
	Body string
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("indexer returned HTTP %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("indexer error %d: %s", e.Code, e.Body)
}

// RateLimited reports whether the indexer refused the request.
//
// Callers turn this into a non-retryable failure with a cooldown rather than
// retrying: hammering an indexer that has just refused is how an IP gets
// banned.
func (e *Error) RateLimited() bool {
	return e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode == http.StatusServiceUnavailable ||
		e.Code == 500 // Torznab's "request limit reached"
}

// Unauthorized reports whether the API key was rejected.
func (e *Error) Unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

func joinInts(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
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

type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
	Size        int64  `xml:"size"`
	Description string `xml:"description"`
	Enclosure   struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
	Attrs []torznabAttr `xml:"http://torznab.com/schemas/2015/feed attr"`
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (i rssItem) toRelease(indexerName string) domain.Release {
	attrs := make(map[string]string, len(i.Attrs))
	categories := make([]int, 0, 2)

	for _, a := range i.Attrs {
		// Repeating an attribute name with a different value is how this
		// format expresses a list, and category is the one that really is a
		// list — a release can sit in both "Movies" and "Movies/Bluray". So
		// categories are collected across every attribute rather than read
		// from the collapsed map, which would keep only the first.
		if a.Name == "category" {
			if v, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil {
				categories = append(categories, v)
			}
			continue
		}

		// Everything else is scalar, and a duplicate is an indexer bug: the
		// first value wins.
		if _, seen := attrs[a.Name]; !seen {
			attrs[a.Name] = a.Value
		}
	}

	rel := domain.Release{
		GUID:        i.GUID,
		InfoHash:    strings.ToLower(attrs["infohash"]),
		Title:       strings.TrimSpace(i.Title),
		SizeBytes:   i.Size,
		Indexer:     indexerName,
		DownloadURL: i.Enclosure.URL,
		MagnetURI:   attrs["magneturl"],
		Categories:  categories,
		Attrs:       attrs,
	}

	// size is a required attribute but some indexers put it only in the RSS
	// element, and some only in the attribute. Either is acceptable.
	if rel.SizeBytes == 0 {
		if v, err := strconv.ParseInt(attrs["size"], 10, 64); err == nil {
			rel.SizeBytes = v
		}
	}

	if v, err := strconv.Atoi(attrs["seeders"]); err == nil {
		rel.Seeders = v
	}
	if v, err := strconv.Atoi(attrs["peers"]); err == nil {
		// peers is seeders + leechers, so leechers is the difference.
		rel.Leechers = v - rel.Seeders
		if rel.Leechers < 0 {
			rel.Leechers = 0
		}
	}

	if rel.DownloadURL == "" {
		rel.DownloadURL = i.Link
	}

	// pubDate is RFC1123Z in practice, but the format varies between front
	// ends and some emit something unparseable. A missing date is not worth
	// failing a search over.
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339} {
		if t, err := time.Parse(layout, i.PubDate); err == nil {
			rel.PublishedAt = t
			break
		}
	}

	return rel
}
