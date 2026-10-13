package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alex/mediad/internal/tmdb"
)

// Posters are served by our own backend rather than linked to TMDB.
//
// The reason is deployment, not taste. Where TMDB is reachable only through a
// proxy, the browser cannot fetch image.tmdb.org: the page renders and every
// card is a broken image. The failure is also invisible from the server's side
// — a failed image load is not something a page reports — so it presents as
// "the search results look wrong" rather than as an error anywhere.
//
// So the browser asks us, and we fetch through the same client and the same
// proxy as every API call.

const (
	// defaultPosterSize is the width used when a page does not name one.
	// w342 is a card; the confirmation page uses w500.
	defaultPosterSize = "w342"

	// posterCacheTTL is how long a browser may keep a poster.
	//
	// A poster at a given path and size is immutable at TMDB — a changed image
	// gets a new path — so this can be long, and the point of caching is that
	// the browser stops asking us at all. Without it every scroll and every
	// three-second fragment swap would re-request every visible image.
	posterCacheTTL = 30 * 24 * time.Hour
)

// posterPath builds the URL the templates use.
//
// It returns an empty string for an empty poster path, which is what makes the
// templates' {{if .PosterURL}} guard work: a title with no poster renders a
// placeholder rather than an <img> pointing at nothing.
func posterPath(path, size string) string {
	if path == "" {
		return ""
	}
	if size == "" {
		size = defaultPosterSize
	}

	// The path is already of the form "/abc123.jpg" and contains no character
	// that needs escaping, but it is escaped anyway: this is a URL built from
	// a value that came from an external service, and "it cannot contain
	// anything odd" is the assumption that stops being true.
	return "/poster/" + url.PathEscape(size) + strings.ReplaceAll(
		url.PathEscape(path), "%2F", "/")
}

// handlePoster streams a poster image from TMDB.
//
// It streams rather than buffering: an "original" poster is a few megabytes,
// and holding one in memory per concurrent viewer to then copy it to a socket
// is work with no purpose. The body is copied straight through.
func (s *Server) handlePoster(w http.ResponseWriter, r *http.Request) {
	// The wildcard arrives as PathValue("*") and without its leading slash,
	// which is why it is put back rather than used as it comes. Reading it as
	// PathValue("path") — the intuitive name — yields the empty string, and
	// the resulting request for "/" fails for every poster with an error that
	// looks like TMDB's fault.
	size := r.PathValue("size")
	path := "/" + strings.TrimPrefix(r.PathValue("*"), "/")

	if path == "/" {
		http.Error(w, "no poster path", http.StatusBadRequest)
		return
	}

	poster, err := s.tmdb.FetchPoster(r.Context(), path, size)
	if err != nil {
		s.posterError(w, r, err, size, path)
		return
	}
	defer poster.Close()

	w.Header().Set("Content-Type", poster.ContentType)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable",
		int(posterCacheTTL.Seconds())))

	// The upstream length is passed on when it is known, so the browser can
	// show progress and size the layout. It is advisory — the body is bounded
	// by the reader, not by this — so a mismatch is not fatal.
	if poster.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(poster.Size, 10))
	}

	if _, err := copyStream(w, poster.Body); err != nil {
		// The headers are already sent, so this cannot become an error
		// response. It is logged because a truncated image is worth knowing
		// about, and the browser will show a partial or broken image, which is
		// the honest outcome.
		s.log.Warn("a poster stream ended early",
			"path", path, "size", size, "error", err)
	}
}

// posterError turns a fetch failure into a response.
//
// The distinction that matters is between "this title has no poster" and "we
// could not reach TMDB": the first is a 404 the browser caches and never asks
// about again, and the second must not be cached, or a transient outage leaves
// the interface permanently convinced that every poster is missing.
func (s *Server) posterError(w http.ResponseWriter, r *http.Request, err error, size, path string) {
	var apiErr *tmdb.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusNotFound:
			// Long-lived, deliberately: TMDB will not grow this image later.
			w.Header().Set("Cache-Control", "public, max-age=86400")
			http.Error(w, "no poster", http.StatusNotFound)
			return

		case apiErr.StatusCode == http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "60")
			http.Error(w, "upstream is rate limiting", http.StatusServiceUnavailable)
			return
		}
	}

	s.log.Warn("could not fetch a poster", "path", path, "size", size, "error", err)

	// no-store, so a failed fetch is not remembered as an answer.
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "poster unavailable", http.StatusBadGateway)
}

// copyStream copies a reader to a writer, flushing as it goes.
//
// The flush is what makes this a stream rather than a buffer with extra steps:
// net/http buffers the response, and without flushing it would hold the whole
// image until the copy finished, which is the memory cost this exists to
// avoid.
func copyStream(w http.ResponseWriter, src interface{ Read([]byte) (int, error) }) (int64, error) {
	buf := make([]byte, 32<<10)

	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			m, writeErr := w.Write(buf[:n])
			written += int64(m)
			if writeErr != nil {
				return written, writeErr
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}
