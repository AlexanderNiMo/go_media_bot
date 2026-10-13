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
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// Jackett is a client for a Jackett aggregate Torznab endpoint.
//
// Jackett's per-indexer endpoints expose the full Torznab surface (imdbid,
// tvdbid, season, per-indexer caps), but the aggregate /all endpoint does
// not: it returns caps that advertise only `q` and refuses `t=movie` and
// `t=tvsearch` with Torznab error 201 ("all does not support the requested
// query"). That is enough of a difference from the general Torznab client
// that it gets its own type — sharing more code would mean sharing the parts
// that lie to the workflow about what the front end can do.
//
// Two deliberate consequences:
//
//   - The query is always free text. A spec that has imdbid and tvdbid
//     still feeds the title into `q`; an indexer that can use ids has to
//     discover them from the release name, which is how the front ends most
//     Russian trackers work.
//   - Caps() is a constant: a real call would be against the empty
//     aggregate caps and produce a degenerate `Caps{}` that the workflow
//     would then mistakenly consult. The aggregate is what the search hits
//     and what caps reports, so they are read from the same source: nothing.
type Jackett struct {
	http    *http.Client
	baseURL string
	apiKey  string
	name    string
	log     *slog.Logger
	limiter *rate.Limiter
}

// NewJackett builds a Jackett client.
//
// The base URL is the Jackett aggregate Torznab path
// (/api/v2.0/indexers/all/results/torznab). Pointing it at a per-indexer
// endpoint would also work, but the aggregate is the single URL the project
// documents and the one any Jackett install produces by default.
func NewJackett(cfg *config.Config, log *slog.Logger) *Jackett {
	return &Jackett{
		http: &http.Client{
			Timeout: 90 * time.Second,
		},
		baseURL: strings.TrimRight(cfg.Indexer.BaseURL, "/"),
		apiKey:  cfg.Indexer.APIKey,
		name:    "jackett",
		log:     log.With("component", "indexer", "mode", "jackett"),
		limiter: rate.NewLimiter(rate.Limit(cfg.Indexer.RatePerSecond), 1),
	}
}

// Name identifies the front end.
func (j *Jackett) Name() string { return j.name }

// Caps returns the aggregate's reported capabilities.
//
// The aggregate caps are a constant of the front end, not a property of any
// particular Jackett install: `t=search` is all the aggregate advertises, and
// any other call mode is rejected at search time. Hard-coding the answer
// here avoids a redundant request against an empty caps document and, more
// importantly, makes it impossible for the workflow to believe the aggregate
// supports identifiers it does not.
func (j *Jackett) Caps(ctx context.Context) (Caps, error) {
	return Caps{
		// Only free-text search is supported by the aggregate; the other
		// capabilities are advertised as unavailable, and a Select that asks
		// the spec for imdbid or tvdbid is a no-op against this front end
		// anyway.
		Search:     Capability{Available: true, Params: map[string]bool{"q": true}},
		MovieQuery: Capability{Available: false, Params: nil},
		TVQuery:    Capability{Available: false, Params: nil},
	}, nil
}

// Search queries the aggregate endpoint.
//
// The aggregate is the only thing we have, and the aggregate only accepts
// `t=search`, so every request looks the same to it. A spec that names a
// season is searched with the season folded into the free-text query — Jackett
// is the wrong place to apply the protocol's "season" parameter.
func (j *Jackett) Search(ctx context.Context, req SearchRequest) ([]domain.Release, error) {
	if j.baseURL == "" {
		return nil, errors.New("jackett base url is not configured")
	}
	if j.apiKey == "" {
		return nil, errors.New("jackett api key is not configured")
	}

	if err := j.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("indexer rate limiter: %w", err)
	}

	query := buildQuery(req)

	params := url.Values{}
	params.Set("t", "search")
	params.Set("q", query)
	if req.Limit > 0 {
		params.Set("limit", strconv.Itoa(req.Limit))
	}
	// Ask for the attribute set explicitly; Jackett has been observed to
	// omit the extended attributes without it, which strips infohash and
	// magneturl.
	params.Set("extended", "1")
	if len(req.Categories) > 0 {
		params.Set("cat", joinInts(req.Categories))
	}

	endpoint := j.baseURL + "/api?" + params.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build jackett request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/xml, text/xml")

	resp, err := j.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call jackett: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorznabBytes))
	if err != nil {
		return nil, fmt.Errorf("read jackett response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &Error{StatusCode: resp.StatusCode, Body: truncate(string(body), 300)}
	}

	var failure struct {
		XMLName xml.Name `xml:"error"`
		Code    int      `xml:"code,attr"`
		Message string   `xml:"description,attr"`
	}
	if err := xml.Unmarshal(body, &failure); err == nil && failure.XMLName.Local == "error" {
		return nil, &Error{Code: failure.Code, Body: failure.Message}
	}

	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("decode jackett response: %w", err)
	}

	releases := make([]domain.Release, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		releases = append(releases, item.toRelease(j.name))
	}
	return releases, nil
}

// buildQuery assembles the free-text query Jackett will send to every
// configured indexer.
//
// Russian trackers match on the release name, not on identifiers; this is
// the union of what the spec has and the parts that a Russian indexer will
// actually key on, in the order the user would type them.
func buildQuery(req SearchRequest) string {
	parts := []string{}
	if req.Query != "" {
		parts = append(parts, req.Query)
	}
	if req.IMDBID != "" {
		// Most Jackett indexers do not match on a bare IMDB id, but a few
		// do, and the cost of including it is one extra token in the
		// string Jackett hands to the tracker.
		parts = append(parts, req.IMDBID)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
