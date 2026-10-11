package domain

import "time"

// Release is one torrent offered by an indexer, normalised across the two
// indexer front ends.
//
// It carries everything the selector scores on and everything the download
// step needs, and nothing else: a release is a value that gets logged, stored
// in the event history and shown to a user, so keeping it small matters.
type Release struct {
	// GUID identifies the release within its indexer. Preferred over the
	// infohash for de-duplication because a magnet-only indexer may not expose
	// one, while every result has a guid.
	GUID string
	// InfoHash is the lower-case hex infohash, when the indexer exposes it.
	// This is what the blacklist is keyed on.
	InfoHash string
	Title    string

	SizeBytes int64
	Seeders   int
	Leechers  int

	// Indexer is the name of the tracker the result came from, as reported by
	// the indexer front end. It is a scoring input, not just a label: a
	// Russian-language tracker naming a release is itself evidence about the
	// audio track.
	Indexer string

	DownloadURL string
	MagnetURI   string

	Categories  []int
	PublishedAt time.Time

	// Attrs carries the raw Torznab attributes. Kept because the set of
	// attributes an indexer emits varies and grows, and the audio detector
	// benefits from seeing whatever was actually reported.
	Attrs map[string]string
}

// Attr returns a Torznab attribute, or the empty string.
func (r Release) Attr(name string) string {
	if r.Attrs == nil {
		return ""
	}
	return r.Attrs[name]
}

// Link returns the best URL to hand a torrent client.
//
// A magnet is preferred when present: it needs no indexer session and cannot
// expire between selection and download, which matters because those two steps
// are separated by a workflow task boundary and possibly a retry.
func (r Release) Link() string {
	if r.MagnetURI != "" {
		return r.MagnetURI
	}
	return r.DownloadURL
}

// Identity returns the key the blacklist and de-duplication use.
func (r Release) Identity() string {
	if r.InfoHash != "" {
		return r.InfoHash
	}
	return r.GUID
}
