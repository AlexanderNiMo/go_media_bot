package domain

import (
	"strconv"
	"testing"
)

func TestWorkflowID(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  MediaRef
		want string
	}{
		{"movie 603", MovieRef(603), "movie-603"},
		{"tv 1399 s2", SeasonRef(1399, 2), "tv-1399-s2"},
		// Specials are season 0, which is why the season is a pointer: with a
		// sentinel this would be indistinguishable from "no season".
		{"tv 1399 s0", SeasonRef(1399, 0), "tv-1399-s0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.WorkflowID(); got != tc.want {
				t.Errorf("WorkflowID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidForRequestRequiresASeason(t *testing.T) {
	// A search hit names a show, not a season. That is a legitimate reference
	// but not something that can be downloaded.
	series := MediaRef{Type: MediaTypeTV, TMDBID: 1399}

	if err := series.Valid(); err != nil {
		t.Errorf("a series reference should be structurally valid: %v", err)
	}
	if err := series.ValidForRequest(); err == nil {
		t.Error("a series reference with no season must not be requestable")
	}

	if err := SeasonRef(1399, 2).ValidForRequest(); err != nil {
		t.Errorf("a season reference should be requestable: %v", err)
	}
	if err := MovieRef(603).ValidForRequest(); err != nil {
		t.Errorf("a film reference should be requestable: %v", err)
	}
}

func TestValidRejectsNonsense(t *testing.T) {
	season := 1
	for name, ref := range map[string]MediaRef{
		"unknown type":       {Type: "book", TMDBID: 1},
		"zero id":            {Type: MediaTypeMovie, TMDBID: 0},
		"negative id":        {Type: MediaTypeMovie, TMDBID: -5},
		"film with a season": {Type: MediaTypeMovie, TMDBID: 1, Season: &season},
		"negative season":    {Type: MediaTypeTV, TMDBID: 1, Season: ptr(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ref.Valid(); err == nil {
				t.Errorf("expected an error")
			}
		})
	}
}

// TestSearchTermsPrefersRussian — the trackers this project targets name
// releases in Russian, so that is the term most likely to match.
func TestSearchTermsPrefersRussian(t *testing.T) {
	spec := MediaSpec{
		Title:         "The Matrix",
		TitleRU:       "Матрица",
		OriginalTitle: "The Matrix",
	}

	terms := spec.SearchTerms()

	if len(terms) != 2 {
		t.Fatalf("terms = %v, want 2 after de-duplication", terms)
	}
	if terms[0] != "Матрица" {
		t.Errorf("first term = %q, want the Russian title", terms[0])
	}
	// The original title is the fallback for a release uploaded under its
	// international name; it must appear once, not twice.
	if terms[1] != "The Matrix" {
		t.Errorf("second term = %q", terms[1])
	}
}

func TestSearchTermsFallsBackToTheOriginal(t *testing.T) {
	// TMDB returns the original string when it has no translation, so a title
	// with no Russian name is the common case, not an edge case.
	spec := MediaSpec{Title: "Some Obscure Film", OriginalTitle: "Some Obscure Film"}

	terms := spec.SearchTerms()
	if len(terms) != 1 || terms[0] != "Some Obscure Film" {
		t.Errorf("terms = %v, want one entry", terms)
	}
}

func TestSearchTermsSkipsEmpty(t *testing.T) {
	spec := MediaSpec{Title: "Only This"}
	if terms := spec.SearchTerms(); len(terms) != 1 {
		t.Errorf("terms = %v, want one entry", terms)
	}

	if terms := (MediaSpec{}).SearchTerms(); len(terms) != 0 {
		t.Errorf("terms = %v, want none", terms)
	}
}

func TestDisplayTitle(t *testing.T) {
	if got := (MediaSpec{Title: "The Matrix", TitleRU: "Матрица"}).DisplayTitle(); got != "Матрица" {
		t.Errorf("DisplayTitle = %q, want the Russian title", got)
	}
	if got := (MediaSpec{Title: "The Matrix"}).DisplayTitle(); got != "The Matrix" {
		t.Errorf("DisplayTitle = %q", got)
	}
}

// TestReleaseLinkPrefersMagnet — the selection and the download are separated
// by a workflow task boundary and possibly a retry, and an indexer download
// URL can expire in between while a magnet cannot.
func TestReleaseLinkPrefersMagnet(t *testing.T) {
	rel := Release{
		DownloadURL: "https://tracker/dl/1.torrent",
		MagnetURI:   "magnet:?xt=urn:btih:abc",
	}
	if got := rel.Link(); got != "magnet:?xt=urn:btih:abc" {
		t.Errorf("Link = %q, want the magnet", got)
	}

	rel.MagnetURI = ""
	if got := rel.Link(); got != "https://tracker/dl/1.torrent" {
		t.Errorf("Link = %q, want the download url", got)
	}
}

// TestReleaseIdentityPrefersInfoHash — the blacklist is keyed on this, and a
// magnet-only indexer may expose no infohash at all, so the guid has to work
// as a fallback rather than producing an empty key.
func TestReleaseIdentityPrefersInfoHash(t *testing.T) {
	rel := Release{GUID: "https://tracker/1", InfoHash: "abc123"}
	if got := rel.Identity(); got != "abc123" {
		t.Errorf("Identity = %q, want the infohash", got)
	}

	rel.InfoHash = ""
	if got := rel.Identity(); got != "https://tracker/1" {
		t.Errorf("Identity = %q, want the guid as a fallback", got)
	}
}

func ptr[T any](v T) *T { return &v }

// TestStoppedDownloadsAreStalled covers a state that only exists in
// qBittorrent 5.x and that was found in a real library: a download sitting at
// 98.6% with the client reporting it as stopped.
//
// This project only ever adds torrents running, so a stopped download is a
// state nobody asked for. Classifying it as "still downloading" would leave
// the request waiting out the full two-day deadline for something that is not
// moving, instead of abandoning it and looking for another release.
func TestStoppedDownloadsAreStalled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   string
		stalled bool
		done    bool
		failed  bool
	}{
		// The 5.x spelling, and the 4.x one for the same state.
		{"stoppeddl", "stoppeddl", true, false, false},
		{"pauseddl", "pauseddl", true, false, false},
		// Already covered, listed so the whole family is in one table.
		{"stalleddl", "stalleddl", true, false, false},
		{"metadl", "metadl", true, false, false},
		{"forcedmetadl", "forcedmetadl", true, false, false},
		// A stopped *upload* is a finished download, not a stalled one: the
		// payload is complete and present.
		{"stoppedup", "stoppedup", false, true, false},
		{"pausedup", "pausedup", false, true, false},
		// Ordinary progress.
		{"downloading", "downloading", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := TorrentStatus{State: tc.state, Progress: 0.5}

			if got := s.Stalled(); got != tc.stalled {
				t.Errorf("state %q: Stalled = %v, want %v", tc.state, got, tc.stalled)
			}
			if got := s.Done(); got != tc.done {
				t.Errorf("state %q: Done = %v, want %v", tc.state, got, tc.done)
			}
			if got := s.Failed(); got != tc.failed {
				t.Errorf("state %q: Failed = %v, want %v", tc.state, got, tc.failed)
			}
		})
	}
}

// TestStoppedIsStalledRegardlessOfProgress — the case in the real library was
// at 98.6%, so progress is not what decides this.
func TestStoppedIsStalledRegardlessOfProgress(t *testing.T) {
	for _, progress := range []float64{0, 0.5, 0.986, 0.999} {
		t.Run(strconv.FormatFloat(progress, 'f', 3, 64), func(t *testing.T) {
			s := TorrentStatus{State: "stoppeddl", Progress: progress}
			if !s.Stalled() {
				t.Errorf("progress %.3f: a stopped download should count as stalled", progress)
			}
			if s.Done() {
				t.Errorf("progress %.3f: a stopped download is not done", progress)
			}
		})
	}
}
