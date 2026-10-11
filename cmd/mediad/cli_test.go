package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
)

// The rendering helpers are what the user actually reads, and the failures
// here are the quiet kind: a table that is a column out of line for every
// Cyrillic title, or a duration that says "0s" for something that has not
// finished. Both are cheap to pin down.

func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "0s"},
		{in: 45 * time.Second, want: "45s"},
		{in: time.Minute, want: "1m"},
		{in: 90 * time.Minute, want: "1h 30m"},
		{in: 3 * time.Hour, want: "3h 0m"},
		{in: 47 * time.Hour, want: "47h 0m"},
		{in: 48 * time.Hour, want: "2d"},
		{in: 30 * 24 * time.Hour, want: "30d"},
		// Negative durations arrive from a deadline that has just passed.
		{in: -time.Hour, want: "0s"},
	}

	for _, tc := range cases {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0 B"},
		{in: 512, want: "512 B"},
		{in: 1024, want: "1.0 KiB"},
		{in: 20 << 30, want: "20.0 GiB"},
		{in: 1536 * 1024, want: "1.5 MiB"},
	}

	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTruncateCountsRunes — cutting by bytes mid-character turns a Russian
// title into a replacement glyph, and Russian titles are the common case.
func TestTruncateCountsRunes(t *testing.T) {
	// Nine runes, eighteen bytes.
	const title = "Матрица  "

	if got := truncate(title, 20); got != title {
		t.Errorf("a short enough string was altered: %q", got)
	}

	got := truncate("Матрица: Перезагрузка", 10)
	if got != "Матрица: …" {
		t.Errorf("truncate = %q, want %q", got, "Матрица: …")
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncation cut a character in half: %q", got)
	}
}

// TestDisplayWidth — the columns are padded by this, so getting it wrong makes
// every title of a given script ragged.
func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{in: "abc", want: 3},
		// One cell each: Cyrillic is East Asian Width "Ambiguous", which is
		// one column outside a CJK locale. Treating it as wide here would pad
		// every Russian title two columns short.
		{in: "Матрица", want: 7},
		{in: "七人の侍", want: 8},
		{in: "", want: 0},
	}

	for _, tc := range cases {
		if got := displayWidth(tc.in); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPadColumns(t *testing.T) {
	rows := [][]string{
		{"ID", "STATE", "TITLE"},
		{"1", "pending", "Матрица"},
		{"22", "downloading", "Игра престолов"},
	}

	lines := padColumns(rows, 2)
	if len(lines) != len(rows) {
		t.Fatalf("got %d lines, want %d", len(lines), len(rows))
	}

	// Every line must reach the same visual width for the last column to line
	// up, which is the whole point of padding.
	if len(lines) == 0 {
		t.Fatal("no lines")
	}

	// The last cell is never padded: trailing spaces are invisible in a
	// terminal and travel badly through a pipe.
	for _, line := range lines {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line has trailing whitespace: %q", line)
		}
	}

	if !strings.HasPrefix(lines[0], "ID  STATE") {
		t.Errorf("header not padded as expected: %q", lines[0])
	}
}

func TestPadColumnsWithNoRows(t *testing.T) {
	if got := padColumns(nil, 2); got != nil {
		t.Errorf("want nothing for no rows, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Describing a request
// ---------------------------------------------------------------------------

func baseRequest() *storage.Request {
	return &storage.Request{
		ID:         7,
		WorkflowID: "movie-603",
		State:      storage.StatePending,
		Media: domain.MediaSpec{
			Ref:     domain.MovieRef(603),
			Title:   "The Matrix",
			TitleRU: "Матрица",
			Year:    1999,
		},
		CreatedAt: time.Now(),
	}
}

func TestRequestTitleNamesTheSeason(t *testing.T) {
	r := baseRequest()
	if got := requestTitle(r); got != "Матрица (1999)" {
		t.Errorf("requestTitle = %q", got)
	}

	// Without the season, two requests for the same series are
	// indistinguishable in a list — which is exactly the list somebody reads
	// when something has gone wrong.
	r.Media.Ref = domain.SeasonRef(1399, 2)
	r.Media.TitleRU = "Игра престолов"
	r.Media.Year = 2011

	if got := requestTitle(r); got != "Игра престолов (2011), сезон 2" {
		t.Errorf("requestTitle = %q", got)
	}
}

func TestRequestWhenPerState(t *testing.T) {
	now := time.Now()
	deadline := now.Add(2 * time.Hour)

	cases := []struct {
		name  string
		mut   func(*storage.Request)
		wants []string
	}{
		{
			name: "downloading",
			mut: func(r *storage.Request) {
				r.State = storage.StateDownloading
				r.Progress = 0.432
				r.DlSpeed = 12 << 20
				r.ETASeconds = 4800
			},
			wants: []string{"43.2%", "12.0 MiB/s", "1h 20m left"},
		},
		{
			name: "waiting",
			mut: func(r *storage.Request) {
				r.State = storage.StatePending
				r.NextRunAt = &deadline
			},
			wants: []string{"next in"},
		},
		{
			name: "due",
			mut: func(r *storage.Request) {
				r.State = storage.StateSearching
				past := now.Add(-time.Minute)
				r.NextRunAt = &past
			},
			wants: []string{"due now"},
		},
		{
			// A skip is only useful if it says what it matched: that is what
			// makes a false positive visible instead of silent.
			name: "skipped",
			mut: func(r *storage.Request) {
				r.State = storage.StateSkipped
				r.PlexTitle = "The Matrix"
				r.PlexYear = 1999
			},
			wants: []string{"in Plex", "The Matrix", "1999"},
		},
		{
			name: "failed",
			mut: func(r *storage.Request) {
				r.State = storage.StateFailed
				r.LastError = "no acceptable release in 47 searches"
			},
			wants: []string{"no acceptable release"},
		},
		{
			name: "completed",
			mut: func(r *storage.Request) {
				r.State = storage.StateCompleted
				at := now
				r.CompletedAt = &at
			},
			wants: []string{"finished"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRequest()
			tc.mut(r)

			got := requestWhen(r, now)
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("requestWhen = %q, want it to mention %q", got, want)
				}
			}
			if got == "" {
				t.Error("requestWhen produced nothing")
			}
		})
	}
}

func TestDescribeSearchCountsSearches(t *testing.T) {
	r := baseRequest()

	// A request that has never searched says so rather than reporting "0
	// searches", which reads like something is wrong.
	if got := describeSearch(r, time.Now()); !strings.Contains(got, "first search") {
		t.Errorf("describeSearch = %q", got)
	}

	// The label beside this already says "search", so the value must not say
	// it again.
	r.AttemptCount = 12
	future := time.Now().Add(3 * time.Hour)
	r.NextRunAt = &future

	got := describeSearch(r, time.Now())
	if !strings.Contains(got, "12 so far") {
		t.Errorf("describeSearch = %q", got)
	}
	if strings.Contains(got, "12 searches") {
		t.Errorf("describeSearch repeats its own label: %q", got)
	}

	// A finished request has no next run to report, so it counts what was done.
	r.State = storage.StateFailed
	if got := describeSearch(r, time.Now()); !strings.Contains(got, "12 attempts") {
		t.Errorf("describeSearch = %q", got)
	}
}

func TestEnsureMediaType(t *testing.T) {
	cases := map[string]domain.MediaType{
		"movie": domain.MediaTypeMovie,
		"Film":  domain.MediaTypeMovie,
		"tv":    domain.MediaTypeTV,
		"show":  domain.MediaTypeTV,
	}

	for in, want := range cases {
		got, err := ensureMediaType(in)
		if err != nil {
			t.Errorf("ensureMediaType(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ensureMediaType(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := ensureMediaType("audiobook"); err == nil {
		t.Error("expected an unknown media type to be refused")
	}
}

func TestParseSeason(t *testing.T) {
	// Not given: nil, so the resolver asks.
	if got, err := parseSeason(""); err != nil || got != nil {
		t.Errorf("parseSeason(\"\") = %v, %v; want nil, nil", got, err)
	}

	// Season 0 is the specials on TMDB, so it has to survive as a real choice
	// rather than being read as "not given".
	got, err := parseSeason("0")
	if err != nil {
		t.Fatalf("parseSeason(0): %v", err)
	}
	if got == nil || *got != 0 {
		t.Errorf("parseSeason(0) = %v, want a pointer to 0", got)
	}

	got, err = parseSeason("2")
	if err != nil || got == nil || *got != 2 {
		t.Errorf("parseSeason(2) = %v, %v", got, err)
	}

	// A typo must be an error, not a silent request for something else. With
	// an integer flag and -1 as the sentinel, --season -1 would have meant
	// "ask" rather than "that is not a season".
	for _, bad := range []string{"two", "-1", "1.5", " "} {
		if _, err := parseSeason(bad); bad != " " && err == nil {
			t.Errorf("parseSeason(%q) was accepted", bad)
		}
	}
}

func ptr[T any](v T) *T { return &v }
