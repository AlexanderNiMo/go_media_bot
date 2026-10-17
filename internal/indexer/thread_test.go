package indexer

import (
	"testing"

	"github.com/alex/mediad/internal/config"
)

// The thread-id parser is the bridge between the indexer payload and
// the workflow's ongoing-season mode. A miss is a silent regression
// in the wrong direction: the release is selected, but the workflow
// cannot follow the thread across updates, and the user gets the
// wrong half of the season.
func TestExtractThreadID(t *testing.T) {
	cases := []struct {
		name        string
		description string
		patterns    []config.ThreadPattern
		want        string
	}{
		// Per-tracker pattern wins over the fallback.
		{
			name:        "rutracker viewtopic.php?t=",
			description: "Torrent: ... http://rutracker.org/forum/viewtopic.php?t=1234567",
			patterns: []config.ThreadPattern{
				{Indexer: "RuTracker", Pattern: `viewtopic\.php\?t=(\d+)`},
			},
			want: "RuTracker:1234567",
		},
		{
			name:        "phpBB with id instead of t",
			description: "http://example.com/forum/viewtopic.php?id=987654",
			patterns: []config.ThreadPattern{
				{Indexer: "Example", Pattern: `viewtopic\.php\?id=(\d+)`},
			},
			want: "Example:987654",
		},
		// Fallback covers the standard phpBB shape when no per-tracker
		// pattern matches.
		{
			name:        "fallback for unmatched indexer",
			description: "see http://tracker.example/forum/viewtopic.php?t=42",
			patterns:    nil,
			want:        "42",
		},
		{
			name:        "fallback with id variant",
			description: "see http://tracker.example/forum/viewtopic.php?id=99",
			patterns:    nil,
			want:        "99",
		},
		// No match anywhere.
		{
			name:        "no link",
			description: "Just a regular description with no thread link.",
			patterns:    nil,
			want:        "",
		},
		{
			name:        "link without id",
			description: "see http://tracker.example/forum/viewtopic.php",
			patterns:    nil,
			want:        "",
		},
		// Misconfigured pattern is dropped silently, not panicking on
		// every search.
		{
			name:        "invalid pattern dropped",
			description: "viewtopic.php?t=1",
			patterns: []config.ThreadPattern{
				{Indexer: "Bad", Pattern: "["},
			},
			want: "1",
		},
		// Non-numeric capture is rejected.
		{
			name:        "non-numeric capture rejected",
			description: "viewtopic.php?t=abc",
			patterns:    nil,
			want:        "",
		},
		// Empty name and empty pattern entries are skipped.
		{
			name:        "empty pattern entries skipped",
			description: "viewtopic.php?t=7",
			patterns: []config.ThreadPattern{
				{Indexer: "", Pattern: `(\d+)`},
				{Indexer: "Anon", Pattern: ""},
			},
			want: "7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileThreadPatterns(tc.patterns)
			_, got := extractThreadID(tc.description, compiled, defaultThreadFallback())
			if got != tc.want {
				t.Errorf("extractThreadID(%q) = %q, want %q", tc.description, got, tc.want)
			}
		})
	}
}

// TestExtractThreadIDReturnsDescription verifies the comment round-trip:
// the description itself is returned unchanged, regardless of whether
// the parse matched or not. The workflow does not display the comment
// today, but having it on the release is what makes it debuggable
// when a thread id does not come out as expected.
func TestExtractThreadIDReturnsDescription(t *testing.T) {
	const desc = "the description"
	compiled := compileThreadPatterns(nil)
	gotDesc, _ := extractThreadID(desc, compiled, defaultThreadFallback())
	if gotDesc != desc {
		t.Errorf("description changed: got %q, want %q", gotDesc, desc)
	}
}
