package request

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// The tests in this file cover the season that is still being released: the
// pack that has some of the episodes, the wait for the rest of them, and the
// update that brings them.

// seasonInput is a request for one season of a series.
func seasonInput() Input {
	in := movieInput()
	in.Spec = domain.MediaSpec{
		Ref:           domain.SeasonRef(1399, 2),
		Title:         "Game of Thrones",
		TitleRU:       "Игра престолов",
		OriginalTitle: "Game of Thrones",
		Year:          2011,
		RuntimeMin:    60,
	}
	in.Policy.SeasonAiringWindow = 21 * 24 * time.Hour
	in.Policy.SeasonRecentWindow = 30 * 24 * time.Hour
	in.Policy.SeasonMonitorWindow = 56 * 24 * time.Hour
	in.Policy.SeasonMonitorInterval = 24 * time.Hour
	return in
}

// seasonSpec is the spec of one season of a series.
func seasonSpec() domain.MediaSpec {
	return domain.MediaSpec{
		Ref:           domain.SeasonRef(1399, 2),
		Title:         "Game of Thrones",
		TitleRU:       "Игра престолов",
		OriginalTitle: "Game of Thrones",
		Year:          2011,
		RuntimeMin:    60,
	}
}

// TestTrackedPackCountsTheEpisodesByName — the count an update is compared
// against has to come from the pack's name, because that is where the
// update's count comes from too.
//
// The torrent client's file count is the truth about the content, but a pack
// whose name claims ten episodes while its torrent carries eight video files
// would look like it has more episodes than the pack it is being compared
// against — and the watcher would delete it and add it again, for as long as
// the season lasts.
func TestTrackedPackCountsTheEpisodesByName(t *testing.T) {
	state := &State{
		Selected:           &domain.Release{Title: "Игра престолов S02 1080p (1-10 из 10)"},
		SelectedVideoCount: 8,
		SelectedThreadID:   "rutracker:12345",
	}

	tracked := trackedPack(state)
	if tracked.Episodes != 10 {
		t.Errorf("episodes = %d, want 10 (the count from the name)", tracked.Episodes)
	}
	if release.IsUpdateOf(*state.Selected, tracked) {
		t.Error("the pack was recognised as an update of itself")
	}
}

// seasonRelease is a release from a forum thread, which is what makes it
// recognisable as an update of another one.
func seasonRelease(title, threadID string) domain.Release {
	r := testRelease(title)
	r.ThreadID = threadID
	return r
}

// runningSeason is a season with eight episodes aired and the last of them
// aired today.
func runningSeason() SeasonFactsResult {
	aired := time.Now()
	return SeasonFactsResult{AiredCount: 8, NewestAirDate: &aired}
}

// TestPartialSeasonWaitsForTheUpdate is the case the feature exists for: a
// pack of eight episodes out of a season that is still running.
//
// The request downloads the pack, waits for the update, and takes the update
// when it appears. The old torrent goes without its files — the episodes
// already on disk are the ones the update repeats.
func (s *WorkflowSuite) TestPartialSeasonWaitsForTheUpdate() {
	s.expectNoise()
	s.expectPlexAbsent()

	var factsCalls int
	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SeasonFactsInput) (SeasonFactsResult, error) {
			factsCalls++
			if factsCalls <= 2 {
				// The check after the first add, and the one in the
				// monitoring branch: the season is still running.
				return runningSeason(), nil
			}
			// The update brought the last two episodes, and the season
			// is over.
			return SeasonFactsResult{AiredCount: 10, PlannedCount: 10}, nil
		}, nil)

	var fileCalls int
	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ VideoFileCountInput) (VideoFileCountResult, error) {
			fileCalls++
			if fileCalls == 1 {
				return VideoFileCountResult{Count: 8}, nil
			}
			return VideoFileCountResult{Count: 10}, nil
		}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-10 из 10)", "rutracker:12345")),
			Summary: "found the update",
		}, nil)

	var adds int
	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ AddTorrentInput) (AddTorrentResult, error) {
			adds++
			return AddTorrentResult{Hash: "hash-" + string(rune('0'+adds))}, nil
		}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone}, nil)

	// The old torrent is removed without its files.
	var deleted []DeleteTorrentInput
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			deleted = append(deleted, in)
			return nil
		})

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	var result Result
	s.NoError(s.env.GetWorkflowError())
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)

	// The pack was downloaded, and so was the update.
	s.Equal(2, adds)

	// The old torrent went first, and without its files: the episodes
	// already downloaded stay on disk and the update reuses them.
	s.Require().Len(deleted, 1)
	s.Equal("hash-1", deleted[0].Hash)
	s.False(deleted[0].DeleteFiles, "the downloaded episodes must stay on disk")
}

// TestSeasonThatIsOverIsNotWaitedFor — a pack that is short of a season that
// ended years ago has nothing to wait for. The request finishes with what it
// has.
func (s *WorkflowSuite) TestSeasonThatIsOverIsNotWaitedFor() {
	s.expectNoise()
	s.expectPlexAbsent()

	longAgo := time.Now().AddDate(-2, 0, 0)
	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(SeasonFactsResult{AiredCount: 10, NewestAirDate: &longAgo, AirDate: &longAgo}, nil)

	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(VideoFileCountResult{Count: 8}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "hash-1"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone}, nil)

	// The monitoring branch must never be reached.
	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(SearchResult{Summary: "no update"}, nil).Maybe()

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	var result Result
	s.NoError(s.env.GetWorkflowError())
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.NotContains(result.Reason, "not completed")
}

// TestSeasonWaitEndsWhenTheTrackerGoesQuiet — the wait has a deadline. A
// tracker publishes the rest of a season days after the last episode, and a
// request that waits for years is a request nobody looks at.
//
// The season is running when the pack is added — that is what starts the wait
// — and by the time the request looks for the update, the last episode has
// been out for long enough that the tracker is never going to publish it.
func (s *WorkflowSuite) TestSeasonWaitEndsWhenTheTrackerGoesQuiet() {
	s.expectNoise()
	s.expectPlexAbsent()

	var factsCalls int
	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SeasonFactsInput) (SeasonFactsResult, error) {
			factsCalls++
			if factsCalls == 1 {
				// The check after the add: the season is running.
				return runningSeason(), nil
			}
			// The check in the monitoring branch: the last episode aired
			// eight weeks and a half ago, so the wait is over.
			aired := time.Now().Add(-59 * 24 * time.Hour)
			return SeasonFactsResult{AiredCount: 8, NewestAirDate: &aired, AirDate: &aired}, nil
		}, nil)

	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(VideoFileCountResult{Count: 8}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "hash-1"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone}, nil)

	// The tracker never publishes an update.
	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(SearchResult{Summary: "no update"}, nil).Maybe()

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	var result Result
	s.NoError(s.env.GetWorkflowError())
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.Contains(result.Reason, "not completed")
}

// TestSeasonWaitSurvivesAnIndexerFailure — an indexer outage must not end a
// wait that has weeks to run.
func (s *WorkflowSuite) TestSeasonWaitSurvivesAnIndexerFailure() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(runningSeason(), nil)

	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(VideoFileCountResult{Count: 8}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "hash-1"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone}, nil)

	var updates int
	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ UpdateSearchInput) (SearchResult, error) {
			updates++
			if updates == 1 {
				return SearchResult{}, errIndexer
			}
			return SearchResult{Summary: "no update"}, nil
		}, nil)

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	// The wait continues: the tracker is asked again after the failure, and
	// again after that, until the request's own deadline arrives.
	s.True(updates >= 2, "the tracker must be asked again after the failure")
}

// errIndexer is a stand-in for an indexer that is down.
var errIndexer = &indexerErrorStub{}

type indexerErrorStub struct{}

func (e *indexerErrorStub) Error() string { return "the indexer is unreachable" }

// TestCancelWhileWaitingKeepsTheEpisodes — the pack of a running season is
// finished and in Plex by the time the request starts waiting for the rest
// of it. Cancelling the request must not take those episodes off the disk.
func (s *WorkflowSuite) TestCancelWhileWaitingKeepsTheEpisodes() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(runningSeason(), nil)

	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(VideoFileCountResult{Count: 8}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "hash-1"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone}, nil)

	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(SearchResult{Summary: "no update"}, nil).Maybe()

	var deleted []DeleteTorrentInput
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			deleted = append(deleted, in)
			return nil
		}).Maybe()

	// Two days in: the pack is downloaded and the request is waiting.
	s.env.RegisterDelayedCallback(func() {
		s.env.CancelWorkflow()
	}, 48*time.Hour)

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCanceled, result.Outcome)

	s.Require().Len(deleted, 1, "the torrent was not removed while cancelling")
	s.False(deleted[0].DeleteFiles,
		"the episodes that are already in Plex must stay on the disk")
}

// TestStalledUpdateKeepsTheEpisodes — the update repeats the episodes that
// are already downloaded, so abandoning it must not delete them.
func (s *WorkflowSuite) TestStalledUpdateKeepsTheEpisodes() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SeasonFacts", mock.Anything, mock.Anything).
		Return(runningSeason(), nil)

	s.env.OnActivity("VideoFileCount", mock.Anything, mock.Anything).
		Return(VideoFileCountResult{Count: 8}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-8 из 10)", "rutracker:12345")),
			Summary: "chose the partial pack",
		}, nil)

	s.env.OnActivity("SearchSeasonUpdates", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(seasonRelease("Игра престолов S02 1080p (1-10 из 10)", "rutracker:12345")),
			Summary: "found the update",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "hash-1"}, nil)

	// The pack downloads; the update never gets off the ground.
	var polls int
	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ CheckDownloadInput) (CheckDownloadResult, error) {
			polls++
			if polls == 1 {
				return CheckDownloadResult{State: DownloadDone}, nil
			}
			return CheckDownloadResult{State: DownloadStalled, Detail: "no progress"}, nil
		}, nil)

	var deleted []DeleteTorrentInput
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			deleted = append(deleted, in)
			return nil
		}).Maybe()

	s.env.ExecuteWorkflow(Workflow, seasonInput())

	s.Require().NotEmpty(deleted, "the stalled torrent was not removed")
	for _, d := range deleted {
		s.False(d.DeleteFiles,
			"the episodes that are already in Plex must stay on the disk")
	}
}
