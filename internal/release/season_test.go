package release

import (
	"testing"
	"time"

	"github.com/alex/mediad/internal/domain"
)

// windows are the deadlines the tests use. They match the shipped defaults.
func windows() SeasonWindows {
	return SeasonWindows{Airing: 21 * 24 * time.Hour, Recent: 30 * 24 * time.Hour}
}

// at is a helper for building air dates in the table tests.
func at(daysAgo int) *time.Time {
	t := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -daysAgo)
	return &t
}

// TestSeasonStateIsLive — the difference between a season that is still
// running and one that is over is what decides whether a request waits.
func TestSeasonStateIsLive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state SeasonState
		want  bool
	}{
		{
			name:  "an episode aired yesterday",
			state: SeasonState{AiredCount: 8, NewestAirDate: at(1)},
			want:  true,
		},
		{
			name:  "the last episode aired two months ago",
			state: SeasonState{AiredCount: 8, NewestAirDate: at(60), AirDate: at(120)},
			want:  false,
		},
		{
			name:  "the season premiered last week",
			state: SeasonState{AiredCount: 1, NewestAirDate: at(7), AirDate: at(7)},
			want:  true,
		},
		{
			name:  "the season premiered three months ago",
			state: SeasonState{AiredCount: 8, NewestAirDate: at(90), AirDate: at(90)},
			want:  false,
		},
		{
			name:  "more episodes are planned than have aired",
			state: SeasonState{AiredCount: 8, PlannedCount: 12, NewestAirDate: at(30), AirDate: at(90)},
			want:  true,
		},
		{
			name:  "nothing is known",
			state: SeasonState{},
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			if got := tc.state.IsLive(now, windows()); got != tc.want {
				t.Errorf("IsLive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSeasonStateNeedsMore — the question the request asks about the pack it
// just downloaded.
func TestSeasonStateNeedsMore(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		state   SeasonState
		files   int
		claimed int
		want    bool
	}{
		{
			name:    "the pack says the season has ten and it has eight",
			state:   SeasonState{AiredCount: 8, NewestAirDate: at(3)},
			files:   8,
			claimed: 10,
			want:    true,
		},
		{
			name:    "the pack says it has all ten and it does",
			state:   SeasonState{AiredCount: 10, NewestAirDate: at(3)},
			files:   10,
			claimed: 10,
			want:    false,
		},
		{
			name:  "the season is planned for twelve and the pack has eight",
			state: SeasonState{AiredCount: 8, PlannedCount: 12},
			files: 8,
			want:  true,
		},
		{
			name:  "the season is planned for twelve and the pack has twelve",
			state: SeasonState{AiredCount: 12, PlannedCount: 12},
			files: 12,
			want:  false,
		},
		{
			name:  "the season is over and the pack is short",
			state: SeasonState{AiredCount: 10, NewestAirDate: at(90), AirDate: at(120)},
			files: 8,
			want:  true,
		},
		{
			name:  "the season is over and the pack is complete",
			state: SeasonState{AiredCount: 10, NewestAirDate: at(90), AirDate: at(120)},
			files: 10,
			want:  false,
		},
		{
			name:  "the season is running and its size is unknown",
			state: SeasonState{AiredCount: 8, NewestAirDate: at(2)},
			files: 8,
			want:  true,
		},
		{
			name:  "nothing is known about the season",
			state: SeasonState{},
			files: 8,
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.state.NeedsMore(tc.files, tc.claimed, now, windows())
			if got != tc.want {
				t.Errorf("NeedsMore = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsUpdateOf — the rule that decides whether a release from the indexer
// is the tracked pack with more episodes in it.
func TestIsUpdateOf(t *testing.T) {
	tracked := Tracked{ThreadID: "rutracker:12345", Episodes: 8, Title: "Show S02 1080p (1-8 из 10)"}

	for _, tc := range []struct {
		name      string
		candidate domain.Release
		want      bool
	}{
		{
			name:      "the same thread with more episodes",
			candidate: domain.Release{Title: "Show S02 1080p (1-10 из 12)", ThreadID: "rutracker:12345"},
			want:      true,
		},
		{
			name:      "the same thread with the same number of episodes",
			candidate: domain.Release{Title: "Show S02 1080p (1-8 из 10)", ThreadID: "rutracker:12345"},
			want:      false,
		},
		{
			name:      "a different thread",
			candidate: domain.Release{Title: "Show S02 1080p (1-10 из 12)", ThreadID: "rutracker:99999"},
			want:      false,
		},
		{
			name:      "a name that says nothing about episodes",
			candidate: domain.Release{Title: "Show S02 1080p WEB-DL", ThreadID: "rutracker:12345"},
			want:      false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUpdateOf(tc.candidate, tracked); got != tc.want {
				t.Errorf("IsUpdateOf = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsUpdateOfNeedsATrackedThread — without a thread there is nothing to
// match against, and a release from anywhere would look like an update.
func TestIsUpdateOfNeedsATrackedThread(t *testing.T) {
	tracked := Tracked{Episodes: 8}
	candidate := domain.Release{Title: "Show S02 1080p (1-10 из 12)"}

	if IsUpdateOf(candidate, tracked) {
		t.Error("a release was recognised as an update without a thread to match")
	}
}

// relInThread is a release that came from a particular forum thread.
func relInThread(title, threadID string, sizeGB float64, seeders int) domain.Release {
	r := rel(title, sizeGB, seeders)
	r.ThreadID = threadID
	return r
}

// TestSelectUpdate — the selection a request waiting for the rest of a season
// makes. It has to pick the update and refuse everything else, including
// releases that would be perfectly acceptable for an ordinary request.
func TestSelectUpdate(t *testing.T) {
	s := selectorFor(t)
	tracked := Tracked{ThreadID: "rutracker:12345", Episodes: 8, Title: "Show S02 1080p (1-8 из 10)"}

	verdict := s.SelectUpdate(seasonSpec(), []domain.Release{
		relInThread("Show S02 1080p (1-10 из 12)", "rutracker:12345", 25, 100),
		relInThread("Show S02 720p (1-12 из 12)", "rutracker:12345", 12, 100),
		relInThread("Show S02 1080p (1-8 из 10)", "rutracker:12345", 20, 100),
		rel("Show S02 1080p (1-10 из 12)", 25, 100),
	}, nil, tracked)

	if !verdict.Acceptable {
		t.Fatalf("no update was chosen: %s", verdict.Summary())
	}
	if verdict.Chosen.Title != "Show S02 1080p (1-10 из 12)" {
		t.Errorf("chose %q", verdict.Chosen.Title)
	}
}

// TestSelectPrefersTheCompleteSeason — a tracker publishes the finished
// season and the episodes of one that is still running. The pack with more
// episodes is the one that finishes the request.
//
// The partial pack here is the better release on every other count — it is a
// remux where the complete one is a WEB-DL — so the test fails without the
// episode bonus.
func TestSelectPrefersTheCompleteSeason(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов S02 1080p BDRemux (1-8 из 10)", 40, 100),
		rel("Игра престолов S02 1080p WEB-DL (1-10 из 10)", 25, 100),
	}, nil, "")

	if !verdict.Acceptable {
		t.Fatalf("nothing was chosen: %s", verdict.Summary())
	}
	if verdict.Chosen.Title != "Игра престолов S02 1080p WEB-DL (1-10 из 10)" {
		t.Errorf("chose %q, want the pack with the whole season", verdict.Chosen.Title)
	}
}

// TestSelectKeepsTheBonusWhenEveryPackIsComplete — the episode bonus is
// relative to the other candidates, so a season where every pack carries the
// whole thing is ranked exactly as it was before.
func TestSelectKeepsTheBonusWhenEveryPackIsComplete(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов S02 1080p (1-10 из 10)", 25, 100),
		rel("Игра престолов S02 1080p (1-10 из 10)", 25, 100),
	}, nil, "")

	if !verdict.Acceptable {
		t.Fatalf("nothing was chosen: %s", verdict.Summary())
	}
}

// TestSelectDoesNotPenaliseAnUnnamedPack — a pack whose name says nothing
// about its episodes might be the whole season, so it must not lose to a
// pack that names itself and carries fewer episodes.
func TestSelectDoesNotPenaliseAnUnnamedPack(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов S02 1080p WEB-DL (1-8 из 10)", 20, 50),
		rel("Игра престолов S02 1080p WEB-DL", 25, 200),
	}, nil, "")

	if !verdict.Acceptable {
		t.Fatalf("nothing was chosen: %s", verdict.Summary())
	}
	if verdict.Chosen.Title != "Игра престолов S02 1080p WEB-DL" {
		t.Errorf("chose %q, want the pack that does not name its episodes", verdict.Chosen.Title)
	}
}

// TestSelectUpdateKeepsTheHardFilters — an update that is a cam rip is not
// an update worth downloading.
func TestSelectUpdateKeepsTheHardFilters(t *testing.T) {
	s := selectorFor(t)
	tracked := Tracked{ThreadID: "rutracker:12345", Episodes: 8}

	verdict := s.SelectUpdate(seasonSpec(), []domain.Release{
		relInThread("Show S02 CAM (1-10 из 12)", "rutracker:12345", 25, 100),
	}, nil, tracked)

	if verdict.Acceptable {
		t.Fatal("a cam rip was accepted as an update")
	}
	if verdict.Rejected[0].Reason != ReasonExcludedSource {
		t.Errorf("reason = %q, want %q", verdict.Rejected[0].Reason, ReasonExcludedSource)
	}
}

// TestSelectUpdateRejectsWhatItIsNotWaitingFor — a release that is not an
// update is refused with its own reason, so the audit trail says why.
func TestSelectUpdateRejectsWhatItIsNotWaitingFor(t *testing.T) {
	s := selectorFor(t)
	tracked := Tracked{ThreadID: "rutracker:12345", Episodes: 8}

	verdict := s.SelectUpdate(seasonSpec(), []domain.Release{
		rel("Show S02 1080p (1-10 из 12)", 25, 100),
	}, nil, tracked)

	if verdict.Acceptable {
		t.Fatal("a release from another thread was accepted as an update")
	}
	if verdict.Rejected[0].Reason != ReasonNotAnUpdate {
		t.Errorf("reason = %q, want %q", verdict.Rejected[0].Reason, ReasonNotAnUpdate)
	}
}
