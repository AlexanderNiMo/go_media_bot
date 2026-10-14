package request

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
)

// The time skipping in this suite is the whole reason stage 4 is testable at
// all: a request that waits three weeks and then downloads for six hours runs
// here in milliseconds.
//
// It comes with one condition that has to be respected, and it is easy to miss.
// The test environment only skips time while no activity is running — when a
// long timer overlaps an in-flight activity it arms a real wall-clock timer for
// the remainder. So every activity that would take real time is mocked.
// An unmocked sleeping activity would make these tests take as long as the
// scenario they describe.

func testPolicy() Policy {
	return Policy{
		SearchDeadline:   30 * 24 * time.Hour,
		BackoffLadder:    []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 12 * time.Hour, 24 * time.Hour},
		Jitter:           0, // deterministic in tests
		PollInterval:     5 * time.Minute,
		ContinueAsNewAt:  5000,
		DownloadDeadline: 48 * time.Hour,
		ZeroStallAfter:   45 * time.Minute,
		MissingGrace:     3 * time.Minute,
		MaxStalls:        3,
	}
}

func movieInput() Input {
	return Input{
		RequestID: 1,
		Spec: domain.MediaSpec{
			Ref:           domain.MovieRef(603),
			Title:         "The Matrix",
			TitleRU:       "Матрица",
			OriginalTitle: "The Matrix",
			Year:          1999,
			RuntimeMin:    136,
		},
		Policy: testPolicy(),
	}
}

// release returns a release that the selector logic would accept, expressed
// here as a plain value because the activities are mocked.
func testRelease(title string) domain.Release {
	return domain.Release{
		GUID:        "guid-1",
		InfoHash:    "abc123def456",
		Title:       title,
		SizeBytes:   20 << 30,
		Seeders:     50,
		Indexer:     "RuTracker",
		DownloadURL: "https://tracker/dl/1",
		MagnetURI:   "magnet:?xt=urn:btih:abc123def456",
	}
}

type WorkflowSuite struct {
	// suite.Suite supplies the assertions; the embedded test suite supplies
	// the Temporal test environment and its NewTestWorkflowEnvironment.
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

func TestWorkflowSuite(t *testing.T) {
	suite.Run(t, new(WorkflowSuite))
}

func (s *WorkflowSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()

	// Wall-clock bound, not workflow-time bound: it exists to fail a test that
	// deadlocks rather than to limit how far the simulated clock runs.
	s.env.SetTestTimeout(30 * time.Second)
	// The simulated horizon. The mock clock skips past this, so a workflow
	// that would run for longer than the policy allows fails here.
	s.env.SetWorkflowRunTimeout(40 * 24 * time.Hour)

	s.env.RegisterActivity(&Activities{})
}

func (s *WorkflowSuite) AfterTest(_, _ string) {
	s.env.AssertExpectations(s.T())
}

// expectPlexAbsent stubs a Plex check that finds nothing.
func (s *WorkflowSuite) expectPlexAbsent() {
	s.env.OnActivity("CheckPlex", mock.Anything, mock.Anything).
		Return(CheckPlexResult{Checked: true}, nil)
}

// expectPlexPresent stubs a Plex check that finds the media.
func (s *WorkflowSuite) expectPlexPresent(title string, year int) {
	s.env.OnActivity("CheckPlex", mock.Anything, mock.Anything).
		Return(CheckPlexResult{InPlex: true, Title: title, Year: year, Checked: true}, nil)
}

// expectNoise stubs the bookkeeping activities every path calls.
func (s *WorkflowSuite) expectNoise() {
	s.env.OnActivity("SaveState", mock.Anything, mock.Anything).
		Return(nil).Maybe()
	s.env.OnActivity("NotifyWatchers", mock.Anything, mock.Anything).
		Return(nil).Maybe()
	s.env.OnActivity("RefreshPlex", mock.Anything, mock.Anything).
		Return(nil).Maybe()
	// PopManualLink is called on every search tick. The default behaviour
	// is "no manual link pending" so the workflow falls through to the
	// indexer path; tests that want a manual link override this stub.
	s.env.OnActivity("PopManualLink", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ int64) (string, error) {
			return "", nil
		}).Maybe()
}

// expectDelete stubs the torrent removal and returns a counter of the calls.
//
// It is deliberately not part of expectNoise. testify matches the first
// registered expectation, so a catch-all DeleteTorrent registered there would
// silently win over any test that wants to observe the call — and a test that
// asserts on a counter which never runs passes for the wrong reason.
func (s *WorkflowSuite) expectDelete() *int {
	calls := new(int)
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			*calls++
			return nil
		})
	return calls
}

// ---------------------------------------------------------------------------
// Plex
// ---------------------------------------------------------------------------

// TestAlreadyInPlex is the cheapest possible outcome and the one that must
// never search: asking the library costs one request, searching costs a
// fan-out across every tracker.
func (s *WorkflowSuite) TestAlreadyInPlex() {
	s.expectNoise()
	s.expectPlexPresent("The Matrix", 1999)

	// No search activity is stubbed, so if the workflow searched, the test
	// would fail on an unexpected call.
	s.env.ExecuteWorkflow(Workflow, movieInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateSkipped, result.Outcome)
	s.Equal("The Matrix", result.PlexTitle)
	s.Equal(1999, result.PlexYear)
}

// TestPlexAppearsDuringSearch covers the case that makes the second check worth
// its cost: another tool grabbed the same media while this request was waiting.
func (s *WorkflowSuite) TestPlexAppearsDuringSearch() {
	s.expectNoise()

	var plexCalls int
	s.env.OnActivity("CheckPlex", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ CheckPlexInput) (CheckPlexResult, error) {
			plexCalls++
			if plexCalls == 1 {
				return CheckPlexResult{Checked: true}, nil
			}
			return CheckPlexResult{InPlex: true, Title: "The Matrix", Year: 1999, Checked: true}, nil
		}, nil)

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:     ptr(testRelease("Матрица (1999) 1080p BDRemux | Дубляж")),
			Considered: 5,
			Summary:    "chose one",
		}, nil)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateSkipped, result.Outcome)
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// TestSearchesUntilFound is the core scenario, and the one that shows what the
// time skipping buys: a release that appears on day three is found in a test
// that runs in milliseconds.
func (s *WorkflowSuite) TestSearchesUntilFound() {
	s.expectNoise()
	s.expectPlexAbsent()

	var searches int
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			searches++
			if searches < 4 {
				return SearchResult{Considered: 10, Summary: "nothing acceptable"}, nil
			}
			return SearchResult{
				Chosen:     ptr(testRelease("Матрица (1999) 1080p BDRemux | Дубляж")),
				Considered: 10,
				Summary:    "chose one",
			}, nil
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil)

	start := s.env.Now()
	s.env.ExecuteWorkflow(Workflow, movieInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.Equal(3, result.Attempts)

	// The simulated clock advanced by the first three rungs of the ladder —
	// one hour, then two, then four — plus one poll interval for the download.
	// Asserting on it is what proves the backoff is applied rather than the
	// loop spinning.
	s.Equal(7*time.Hour+testPolicy().PollInterval, s.env.Now().Sub(start))
}

// TestBackoffLadderIsFollowed — the ladder is what bounds the request rate
// over two years. Without it, a strict hourly schedule would be seventeen
// thousand searches per request.
func (s *WorkflowSuite) TestBackoffLadderIsFollowed() {
	s.expectNoise()
	s.expectPlexAbsent()

	var firedAt []time.Duration
	start := time.Time{}

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			if start.IsZero() {
				start = s.env.Now()
			}
			firedAt = append(firedAt, s.env.Now().Sub(start))
			return SearchResult{Considered: 1, Summary: "nothing"}, nil
		}, nil)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	// Release date is unknown and the deadline is thirty days, so the searches
	// run to the end of the ladder and stay there.
	want := []time.Duration{
		0,
		1 * time.Hour,
		3 * time.Hour,  // +2h
		7 * time.Hour,  // +4h
		15 * time.Hour, // +8h
		27 * time.Hour, // +12h
		51 * time.Hour, // +24h
		75 * time.Hour, // +24h again: the ceiling
	}

	if len(firedAt) < len(want) {
		// s.T() rather than a testify assertion: assert.Assertions has no
		// Fatalf, only require does, and this check has to stop the test
		// rather than continue into an index-out-of-range.
		s.T().Fatalf("only %d searches happened, want at least %d", len(firedAt), len(want))
	}
	for i, expected := range want {
		s.InDelta(expected.Seconds(), firedAt[i].Seconds(), 1,
			"search %d fired at %s, want %s", i, firedAt[i], expected)
	}
}

// TestNotYetReleasedDefers is the case that dominates in practice: someone asks
// for a film that is still in cinemas. Without this the loop burns a search an
// hour for months and eventually fails a request that was always going to
// succeed.
func (s *WorkflowSuite) TestNotYetReleasedDefers() {
	s.expectNoise()
	s.expectPlexAbsent()

	in := movieInput()
	future := s.env.Now().Add(60 * 24 * time.Hour)
	in.Spec.ReleasedAt = &future

	var searches int
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			searches++
			return SearchResult{Considered: 0, Summary: "not released"}, nil
		}, nil)

	s.env.ExecuteWorkflow(Workflow, in)

	// A thirty-day deadline and a release two months out: the workflow should
	// have searched a handful of times, not hundreds.
	s.Less(searches, 10, "the not-yet-released case is not deferring the search")

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateFailed, result.Outcome, "it should still expire eventually")
}

// TestIndexerErrorsDoNotBurnAttempts — an indexer outage is our problem, not
// the media's. Burning a search on it would fail a request that was going to
// succeed on its own.
func (s *WorkflowSuite) TestIndexerErrorsDoNotBurnAttempts() {
	s.expectNoise()
	s.expectPlexAbsent()

	var (
		calls   int
		firedAt []time.Duration
		start   = s.env.Now()
	)
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			calls++
			firedAt = append(firedAt, s.env.Now().Sub(start))
			if calls <= 5 {
				return SearchResult{}, errSimulated("indexer is down")
			}
			return SearchResult{
				Chosen:  ptr(testRelease("Матрица (1999) | Дубляж")),
				Summary: "chose one after the outage",
			}, nil
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)
	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)

	// Not one of the five failures counted as a fruitless search: the attempt
	// count reflects real searches only, so the request is no closer to its
	// deadline for having hit a tracker outage.
	s.Equal(0, result.Attempts,
		"indexer failures were counted as search attempts")

	// A workflow-level backoff happened. It is checked as a lower bound rather
	// than as an exact schedule because the activity has its own retry policy,
	// which fires a few times before the error ever reaches the workflow —
	// those retries advance the simulated clock too.
	s.GreaterOrEqual(firedAt[len(firedAt)-1], 5*time.Minute,
		"the workflow did not back off after repeated indexer failures")
}

// TestSearchDeadlineFails — the request has to end, and the user has to be
// told, rather than looking forever.
func (s *WorkflowSuite) TestSearchDeadlineFails() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Considered: 3, Summary: "nothing acceptable"}, nil)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	s.True(s.env.IsWorkflowCompleted())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateFailed, result.Outcome)
	s.Contains(result.Reason, "deadline")
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// TestDownloadPollsUntilDone — the poll loop, with time skipped between polls.
func (s *WorkflowSuite) TestDownloadPollsUntilDone() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{
			Chosen:  ptr(testRelease("Матрица (1999) 1080p BDRemux | Дубляж")),
			Summary: "chose one",
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	var polls int
	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ CheckDownloadInput) (CheckDownloadResult, error) {
			polls++
			if polls < 4 {
				return CheckDownloadResult{State: DownloadRunning, Hash: "abc123def456"}, nil
			}
			return CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil
		}, nil)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.Equal(4, polls)
}

// TestStalledReleaseIsAbandonedAndSearchedAgain — a dead release is not a dead
// request. The torrent is removed, blacklisted, and a different release is
// looked for.
func (s *WorkflowSuite) TestStalledReleaseIsAbandonedAndSearchedAgain() {
	s.expectNoise()
	s.expectPlexAbsent()

	var searches int
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			searches++
			// Every search finds something, so that the only thing that can
			// end this request is the download loop.
			return SearchResult{
				Chosen:  ptr(testRelease("release number " + strconv.Itoa(searches))),
				Summary: "chose one",
			}, nil
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	var polls int
	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ CheckDownloadInput) (CheckDownloadResult, error) {
			// The first two polls stall; the third succeeds. The counter is
			// shared across download attempts on purpose: it makes the first
			// release fail and a later one succeed, which is the behaviour
			// under test.
			polls++
			if polls <= 2 {
				// Blacklist is set because the real activity sets it for a
				// stall: the decision comes from where the evidence is, and
				// this mock stands in for that.
				return CheckDownloadResult{
					State:     DownloadStalled,
					Hash:      "abc123def456",
					Detail:    "no progress for 45m",
					Blacklist: true,
				}, nil
			}
			return CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil
		}, nil)

	var lastDelete DeleteTorrentInput
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			lastDelete = in
			return nil
		})

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.GreaterOrEqual(searches, 2, "a second release should have been searched for")

	// The torrent must be removed and its release blacklisted, or the very
	// next search would pick the same dead release again.
	s.Equal("abc123def456", lastDelete.Hash)
	s.True(lastDelete.DeleteFiles)
	s.NotEmpty(lastDelete.BlacklistFor, "the dead release was not blacklisted")
}

// TestTooManyStallsFails — three dead releases in a row means the content is
// not actually available, and the user should be told rather than quietly
// retried forever.
func (s *WorkflowSuite) TestTooManyStallsFails() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Chosen: ptr(testRelease("dead release")), Summary: "chose one"}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadStalled, Hash: "abc123def456", Detail: "stalled"}, nil)

	s.expectDelete()

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateFailed, result.Outcome)
	s.Contains(result.Reason, "3 releases")
}

// TestDownloadDeadlineAbandonsTheRelease — two days on one torrent and the
// release is treated as dead, because the search loop can still find another.
func (s *WorkflowSuite) TestDownloadDeadlineIsEnforced() {
	s.expectNoise()
	s.expectPlexAbsent()

	in := movieInput()
	// A deadline shorter than one poll interval, so the first poll after the
	// add is already past it.
	in.Policy.DownloadDeadline = time.Minute
	in.Policy.PollInterval = 5 * time.Minute

	var searches int
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ SearchInput) (SearchResult, error) {
			searches++
			if searches == 1 {
				return SearchResult{Chosen: ptr(testRelease("slow release")), Summary: "chose one"}, nil
			}
			return SearchResult{Summary: "nothing"}, nil
		}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadRunning, Hash: "abc123def456"}, nil)

	var deletions int
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(context.Context, DeleteTorrentInput) error {
			deletions++
			return nil
		})

	s.env.ExecuteWorkflow(Workflow, in)

	s.GreaterOrEqual(deletions, 1, "the release was never abandoned")
}

// TestAddFailureSchedulesARetry — a client that refuses a torrent is an
// infrastructure problem, so the release is not blacklisted.
func (s *WorkflowSuite) TestAddFailureSchedulesARetry() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Chosen: ptr(testRelease("some release")), Summary: "chose one"}, nil)

	var adds int
	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ AddTorrentInput) (AddTorrentResult, error) {
			adds++
			if adds == 1 {
				return AddTorrentResult{}, errSimulated("the client is unreachable")
			}
			return AddTorrentResult{Hash: "abc123def456"}, nil
		})

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil)

	// DeleteTorrent is left unstubbed on purpose: an infrastructure failure
	// must not blacklist the release, and a delete call would prove it did.
	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)
	s.Equal(2, adds, "the add should have been retried")
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// TestCancelWhileWaiting — the user changes their mind while the workflow is
// parked in a long backoff. If the wait were a workflow.Sleep it would take up
// to a day to notice.
func (s *WorkflowSuite) TestCancelWhileWaiting() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Considered: 0, Summary: "nothing"}, nil)

	// Ten simulated days in, well past several backoff rungs.
	s.env.RegisterDelayedCallback(func() {
		s.env.CancelWorkflow()
	}, 10*24*time.Hour)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	s.True(s.env.IsWorkflowCompleted())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result),
		"a cancelled request returns a result, not an error")
	s.Equal(storage.StateCanceled, result.Outcome)
}

// TestCancelDuringDownloadRemovesTheTorrent is the compensation that matters:
// leaving the torrent behind is exactly what a user cancelling is trying to
// avoid.
func (s *WorkflowSuite) TestCancelDuringDownloadRemovesTheTorrent() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Chosen: ptr(testRelease("Матрица (1999) | Дубляж")), Summary: "chose one"}, nil)

	s.env.OnActivity("AddTorrent", mock.Anything, mock.Anything).
		Return(AddTorrentResult{Hash: "abc123def456"}, nil)

	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(CheckDownloadResult{State: DownloadRunning, Hash: "abc123def456"}, nil)

	var deleted []DeleteTorrentInput
	s.env.OnActivity("DeleteTorrent", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DeleteTorrentInput) error {
			deleted = append(deleted, in)
			return nil
		}).Maybe()

	// Two hours in, with a five-minute poll interval: mid-download.
	s.env.RegisterDelayedCallback(func() {
		s.env.CancelWorkflow()
	}, 2*time.Hour)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCanceled, result.Outcome)

	s.Len(deleted, 1, "the torrent was not removed while cancelling")
	s.Equal("abc123def456", deleted[0].Hash)
	s.True(deleted[0].DeleteFiles, "the partial files should go too")
}

// TestCancelBeforeAnyDownloadNeedsNoCompensation — cancelling a request that
// never got as far as a torrent should not produce a delete call for nothing.
func (s *WorkflowSuite) TestCancelBeforeAnyDownloadNeedsNoCompensation() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(SearchResult{Considered: 0, Summary: "nothing"}, nil)

	// DeleteTorrent is deliberately left unstubbed, so an unexpected call fails
	// the test rather than being silently allowed.
	s.env.RegisterDelayedCallback(func() {
		s.env.CancelWorkflow()
	}, 3*time.Hour)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCanceled, result.Outcome)
}

// ---------------------------------------------------------------------------
// History growth
// ---------------------------------------------------------------------------

// TestContinueAsNewBoundsHistory is the constraint that makes a two-year
// workflow possible at all.
//
// The server warns at 10 240 events and terminates at 51 200, and termination
// loses the request rather than reporting an error. A long search loop has to
// restart its own history well before that.
//
// This was written expecting the trigger to be untestable, on the grounds that
// the environment does not grow a history and GetCurrentHistoryLength
// therefore reports nothing useful. That is true and beside the point:
// SetContinueAsNewSuggested supplies the server's own signal, and the workflow
// treats it identically to the length comparison. Reaching for the cheaper of
// two equivalent branches is the whole reason the branch is written as an or.
func (s *WorkflowSuite) TestContinueAsNewBoundsHistory() {
	// Registered before expectNoise, because testify matches the first
	// expectation that fits and the bookkeeping stub would otherwise swallow
	// every call before this one saw it.
	var events []string
	s.env.OnActivity("SaveState", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in SaveStateInput) error {
			events = append(events, in.Event)
			return nil
		})
	s.env.OnActivity("NotifyWatchers", mock.Anything, mock.Anything).
		Return(nil).Maybe()
	s.expectPlexAbsent()

	s.env.SetContinueAsNewSuggested(true)

	s.env.ExecuteWorkflow(Workflow, movieInput())

	var restart *workflow.ContinueAsNewError
	s.Require().ErrorAs(s.env.GetWorkflowError(), &restart,
		"the workflow ignored the server's suggestion to restart, so it would "+
			"run into the history limit and be terminated — which loses the request "+
			"rather than reporting anything")

	// The restart is recorded before it happens, because afterwards this
	// history is gone. Without the event, a request that restarts is
	// indistinguishable from one that was never touched.
	s.Contains(events, "workflow.continued_as_new",
		"the restart left no trace in the audit trail")
}

// TestContinueAsNewCarriesState is the part of the history limit that can
// actually go wrong.
//
// A restart passes its arguments and nothing else, so everything the next run
// needs has to be in them. Nothing observable happens when that is wrong: the
// workflow keeps running, having quietly forgotten something.
//
// Getting this wrong is invisible until it matters. A request that restarts
// mid-download and forgets its torrent would add a second copy of the same
// release, and a request that forgets its phase would begin searching again
// from scratch.
func (s *WorkflowSuite) TestContinueAsNewCarriesState() {
	// No Plex expectation: a workflow resuming a download must not re-check
	// the library, and an unexpected call would fail the test.
	s.expectNoise()

	in := movieInput()

	// Exactly the state a restart hands over: mid-download, with a selected
	// release and a torrent already in the client.
	selected := testRelease("Матрица (1999) 1080p BDRemux | Дубляж")
	in.StartPhase = storage.StateDownloading
	in.StartAttempt = 7
	in.StartStallCount = 1
	in.StartSelected = &selected
	in.StartQbitHash = "abc123def456"
	in.StartNextRunAt = s.env.Now()
	in.StartIteration = 300

	// No AddTorrent expectation: a resumed workflow must not add anything.
	// If it does, the test fails on the unexpected call.
	var polls int
	s.env.OnActivity("CheckDownload", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in CheckDownloadInput) (CheckDownloadResult, error) {
			polls++
			s.Equal(int64(1), in.RequestID)
			return CheckDownloadResult{State: DownloadDone, Hash: "abc123def456"}, nil
		})

	// No search either.
	s.env.ExecuteWorkflow(Workflow, in)

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateCompleted, result.Outcome)

	// It resumed the download it was already running.
	s.Equal(1, polls, "the resumed workflow did not poll the download it had in flight")

	// And it carried the counters rather than starting from zero: the attempt
	// count is what the backoff ladder is indexed by, and resetting it would
	// silently restart the schedule from hourly.
	s.Equal(7, result.Attempts, "the attempt count was reset by the restart")
	s.Require().NotNil(result.Selected)
	s.Equal(selected.Identity(), result.Selected.Identity())
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

// errSimulated builds a plain error for a mocked activity to return.
//
// The workflow reads Temporal's ApplicationError message when one is present,
// and a plain error is what an activity would produce before the SDK wraps it,
// so this exercises the fallback path.
func errSimulated(msg string) error { return &simulatedError{msg} }

type simulatedError struct{ msg string }

func (e *simulatedError) Error() string { return e.msg }

// TestContinueAsNewKeepsSearchDeadline is the difference between a request that
// ends and one that disappears.
//
// A ContinueAsNew passes its arguments and nothing else, so the search window
// has to travel in them. When it does not, the restarted workflow computes a
// fresh one from the policy — and every history reset silently buys the request
// another two years.
//
// What that costs is not a late failure but no failure at all. The window is
// then bounded only by WorkflowExecutionTimeout, which terminates the execution
// rather than returning from it: the projection row stays "searching" forever
// and nobody is told anything. The workflow's own deadline is the only ending
// that produces a result.
func (s *WorkflowSuite) TestContinueAsNewKeepsSearchDeadline() {
	s.expectNoise()
	s.expectPlexAbsent()

	// Counted rather than left unmocked. An unmocked activity runs for real
	// here, so a workflow that wrongly searches fails as a thirty-second
	// heartbeat timeout — which is a failure, but one that says nothing about
	// why. Recording the call turns it into an assertion that names the
	// problem.
	var searches int
	s.env.OnActivity("SearchReleases", mock.Anything, mock.Anything).
		Return(func(context.Context, SearchInput) (SearchResult, error) {
			searches++
			return SearchResult{Considered: 3, Summary: "nothing acceptable"}, nil
		}).Maybe()

	in := movieInput()
	in.StartSearchDeadline = s.env.Now().Add(-time.Hour)

	s.env.ExecuteWorkflow(Workflow, in)

	s.True(s.env.IsWorkflowCompleted())

	var result Result
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal(storage.StateFailed, result.Outcome)
	s.Contains(result.Reason, "deadline")
	s.Zero(searches, "a restarted workflow forgot the search deadline it was given "+
		"and searched again, which is how a two-year request becomes an eternal one")
}

// TestContinueAsNewHandsOverSearchDeadline tests the other half of the
// deadline: not that a restarted workflow honours the value, but that the
// restart passes it on in the first place.
//
// Both halves are needed and neither implies the other. A workflow that reads
// a deadline nobody set is as broken as one that sets a deadline nobody reads,
// and the second is the quieter of the two — the request simply never ends,
// until the server terminates it with a timeout that produces no result and no
// notification.
//
// The restart is provoked deliberately. It does not happen on its own here:
// the test environment does not grow a history, so the event-count comparison
// in the workflow never fires. SetContinueAsNewSuggested is the server's own
// signal, which the workflow treats identically — same branch, same
// consequences.
func (s *WorkflowSuite) TestContinueAsNewHandsOverSearchDeadline() {
	s.expectNoise()
	s.expectPlexAbsent()

	s.env.SetContinueAsNewSuggested(true)

	in := movieInput()
	want := s.env.Now().Add(in.Policy.SearchDeadline)

	s.env.ExecuteWorkflow(Workflow, in)

	// The environment ends a top-level run at the restart rather than
	// following it, so the arguments it was handed are the thing to inspect.
	var restart *workflow.ContinueAsNewError
	s.Require().ErrorAs(s.env.GetWorkflowError(), &restart,
		"the workflow did not restart, so nothing about the hand-over was tested")

	var next Input
	s.Require().NoError(converter.GetDefaultDataConverter().FromPayloads(restart.Input, &next),
		"the arguments of the restarted workflow could not be decoded")

	// Compared as instants rather than as time.Time values: the deadline makes
	// a JSON round trip to reach the next run, which drops the location. The
	// instant is what the workflow acts on, so that is what the test asserts.
	s.True(want.Equal(next.StartSearchDeadline),
		"the restarted workflow was not told when the search ends, so it would "+
			"give itself a fresh two years and the request would never reach its deadline: "+
			"want %s, got %s", want, next.StartSearchDeadline)
}
