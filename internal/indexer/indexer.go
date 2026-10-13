// Package indexer queries torrent trackers through a front end.
//
// Two front ends are supported and they differ in more than protocol:
//
//   - Torznab is the standard XML API. Jackett exposes an aggregate endpoint
//     that fans out to every configured tracker in one request; Prowlarr does
//     not, exposing only one endpoint per indexer.
//   - Prowlarr's own JSON search API does aggregate, and returns JSON, so it
//     needs no XML parsing. It is the default for that reason.
//
// Both produce domain.Release, so nothing downstream knows which one is in use.
package indexer

import (
	"context"
	"fmt"
	"log/slog"

	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// Module wires the configured indexer into the fx graph.
var Module = fx.Module("indexer", fx.Provide(New))

// SearchRequest describes what to look for.
//
// The identifiers are advisory: whether they can be used depends on what the
// indexer reports in its capabilities, and a front end that cannot take an
// imdb id silently returns nothing for that query rather than saying so. Caps
// exists to avoid making that mistake.
type SearchRequest struct {
	// Query is free text. Always usable.
	Query string
	// IMDBID is "tt0133093" and applies to films.
	IMDBID string
	// TVDBID applies to series. TMDB ids are not part of the Torznab spec.
	TVDBID int64
	// Season is set when searching for one season of a series.
	Season *int
	// Episode is unused by this project — requests are per season — but is
	// part of the protocol and is passed through when set.
	Episode *int

	Categories []int
	Limit      int
}

// ForMedia builds a search request for a media spec.
//
// The free-text query is always populated even when an id is available: it is
// the fallback for indexers that ignore ids, and a query with both is what the
// protocol expects.
func ForMedia(spec domain.MediaSpec, categories []int, limit int) SearchRequest {
	req := SearchRequest{
		IMDBID:     spec.IMDBID,
		TVDBID:     spec.TVDBID,
		Categories: categories,
		Limit:      limit,
	}
	if terms := spec.SearchTerms(); len(terms) > 0 {
		req.Query = terms[0]
	}
	if spec.Ref.IsSeason() {
		season := spec.Ref.SeasonNumber()
		req.Season = &season
	}
	return req
}

// Caps describes what an indexer can be asked for.
type Caps struct {
	Search     Capability
	MovieQuery Capability
	TVQuery    Capability
}

// Capability is one entry from the indexer's capability list.
type Capability struct {
	Available bool
	// Params holds the supportedParams attribute, split into a set.
	Params map[string]bool
}

// Supports reports whether a parameter name may be sent.
//
// This is the difference between a search that works and one that returns
// nothing at all: an indexer that does not declare imdbid will not error on
// it, it will simply match nothing, and the result is indistinguishable from
// "no releases exist".
func (c Capability) Supports(param string) bool {
	return c.Available && c.Params[param]
}

// Indexer is a torrent indexer front end.
type Indexer interface {
	// Search returns releases matching the request, newest or most relevant
	// first. Implementations must return an empty slice rather than an error
	// when nothing matched: finding nothing is an ordinary outcome.
	Search(ctx context.Context, req SearchRequest) ([]domain.Release, error)

	// Caps returns the indexer's capabilities. It is cached internally; the
	// call is cheap after the first one.
	Caps(ctx context.Context) (Caps, error)

	// Name identifies the front end and the mode, for logs and the read model.
	Name() string
}

// New builds the indexer selected by configuration.
func New(cfg *config.Config, log *slog.Logger) (Indexer, error) {
	switch cfg.Indexer.Mode {
	case "prowlarr":
		return NewProwlarr(cfg, log), nil
	case "torznab":
		return NewTorznab(cfg, log), nil
	case "jackett":
		return NewJackett(cfg, log), nil
	default:
		return nil, fmt.Errorf("indexer.mode: want prowlarr, torznab or jackett, got %q", cfg.Indexer.Mode)
	}
}
