package domain

import "testing"

func TestWorkflowID(t *testing.T) {
	for _, tc := range []struct {
		ref  MediaRef
		want string
	}{
		{MovieRef(603), "movie-603"},
		{SeasonRef(1399, 2), "tv-1399-s2"},
		// Specials are season 0, which is why the season is a pointer: with a
		// sentinel this would be indistinguishable from "no season".
		{SeasonRef(1399, 0), "tv-1399-s0"},
	} {
		if got := tc.ref.WorkflowID(); got != tc.want {
			t.Errorf("WorkflowID() = %q, want %q", got, tc.want)
		}
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
		if err := ref.Valid(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
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
