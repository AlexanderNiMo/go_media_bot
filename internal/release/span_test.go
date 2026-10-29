package release

import "testing"

// TestEpisodeSpan covers the ways a release name states which episodes it
// carries.
//
// The cases that matter most are the ones that must NOT be read as a range:
// a resolution after an episode number and a pair of years are both two
// numbers next to each other, and reading either as an episode range would
// make the request believe it has a complete season.
func TestEpisodeSpan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		known bool
		from  int
		to    int
		total int
	}{
		{
			name:  "SxxExx-Exx",
			in:    "Game of Thrones S02E01-E08 1080p WEB-DL",
			known: true, from: 1, to: 8,
		},
		{
			name:  "Sxx.Exx.Exx",
			in:    "Game.of.Thrones.S02.E01.E08.1080p.WEB-DL",
			known: true, from: 1, to: 8,
		},
		{
			name:  "SxxExx-xx",
			in:    "Game of Thrones S02E01-08 1080p",
			known: true, from: 1, to: 8,
		},
		{
			name:  "SxxExx Exx",
			in:    "Game of Thrones S02E01 E08 1080p",
			known: true, from: 1, to: 8,
		},
		{
			name:  "Exx-Exx without a season",
			in:    "Game of Thrones E01-E08 1080p",
			known: true, from: 1, to: 8,
		},
		{
			name:  "range with a total",
			in:    "Игра престолов 2 сезон 1080p (1-8 из 10)",
			known: true, from: 1, to: 8, total: 10,
		},
		{
			name:  "range with a total and a season marker",
			in:    "Game of Thrones S02 1080p (1-8 из 10)",
			known: true, from: 1, to: 8, total: 10,
		},
		{
			name:  "bare range with a season marker",
			in:    "Game of Thrones S02 1080p 1-8",
			known: true, from: 1, to: 8,
		},
		{
			name:  "a single episode is not a range",
			in:    "Game of Thrones S02E05 1080p",
			known: false,
		},
		{
			name:  "a resolution after an episode is not a range",
			in:    "Game of Thrones S02E01.1080p.WEB-DL",
			known: false,
		},
		{
			name:  "a year range is not a range",
			in:    "Game of Thrones S02 2011-2012 1080p",
			known: false,
		},
		{
			name:  "a bare range without a season marker is not a range",
			in:    "Game of Thrones 1080p 1-8",
			known: false,
		},
		{
			name:  "no episodes at all",
			in:    "Game of Thrones S02 1080p WEB-DL",
			known: false,
		},
		{
			name:  "the x convention",
			in:    "Game.of.Thrones.S02.1x01-1x08.1080p",
			known: true, from: 1, to: 8,
		},
		{
			name:  "a total with a range the other patterns do not match",
			in:    "Game of Thrones S02E01-E08 из 10",
			known: true, from: 1, to: 8, total: 10,
		},
		{
			name:  "an audio track is not a range",
			in:    "Game of Thrones S02 720p WEB-DL 5.1-2.0",
			known: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in).Episodes
			if got.Known != tc.known {
				t.Fatalf("Parse(%q).Episodes.Known = %v, want %v", tc.in, got.Known, tc.known)
			}
			if !tc.known {
				return
			}
			if got.From != tc.from || got.To != tc.to {
				t.Errorf("Parse(%q).Episodes = %d-%d, want %d-%d", tc.in, got.From, got.To, tc.from, tc.to)
			}
			if got.Total != tc.total {
				t.Errorf("Parse(%q).Episodes.Total = %d, want %d", tc.in, got.Total, tc.total)
			}
		})
	}
}

// TestEpisodeSpanCount — the count is what the season logic compares, so a
// span of one episode must count as one and an unknown span as none.
func TestEpisodeSpanCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"a range", "Show S02E01-E08 1080p", 8},
		{"a single episode", "Show S02E05 1080p", 0},
		{"nothing", "Show S02 1080p", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Episodes.Count(); got != tc.want {
				t.Errorf("Parse(%q).Episodes.Count() = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestEpisodeRangeIsNotASingleEpisode — the SxxExx pattern matches the first
// episode of a range, and without this a pack of eight episodes is refused as
// if it were one.
func TestEpisodeRangeIsNotASingleEpisode(t *testing.T) {
	got := Parse("Game of Thrones S02E01-E08 1080p WEB-DL")
	if got.HasEpisode {
		t.Error("a range of episodes was read as a single episode")
	}
	if got.Season != 2 {
		t.Errorf("season = %d, want 2", got.Season)
	}
}
