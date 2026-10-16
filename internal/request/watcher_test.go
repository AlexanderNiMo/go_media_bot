package request

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"

	"github.com/alex/mediad/internal/storage"
)

// interpret is a shorthand for the pure function under test.
func interpret(p storage.DownloadProgress, now time.Time) CheckDownloadResult {
	policy := testPolicy()
	return interpretDownload(p, now, policy.ZeroStallAfter, policy.MissingGrace)
}

// The watcher suites cover the workflow side. What the activity does with a
// real torrent client is a different question and is answered against a real
// one in internal/qbit's live tests, plus the unit tests for the state
// classification.

type WatcherSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

func TestWatcherSuite(t *testing.T) {
	suite.Run(t, new(WatcherSuite))
}

func (s *WatcherSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()
	s.env.SetTestTimeout(30 * time.Second)
	s.env.RegisterActivity(&Activities{})
}

func (s *WatcherSuite) AfterTest(_, _ string) {
	s.env.AssertExpectations(s.T())
}

func watcherInput() WatcherInput {
	return WatcherInput{
		Interval:        30 * time.Second,
		MaxRun:          time.Hour,
		ContinueAsNewAt: 5000,
		GuardDelay:      time.Second,
	}
}

// TestWatcherLoopsForAnHourAtATime is the property that keeps the history
// small, and it is the whole reason the watcher is a separate workflow.
//
// Polling at the interface's cadence from the request workflow would cost about
// fifty thousand history events over a two-day download, against a termination
// limit of fifty-one thousand. Here the fast polling happens inside the
// activity, where it costs nothing, and the workflow only sees one event per
// invocation.
func (s *WatcherSuite) TestWatcherLoopsForAnHourAtATime() {
	var calls int
	s.env.OnActivity("WatchDownloads", mock.Anything, mock.Anything).
		Return(func(_ context.Context, in WatchInput) error {
			calls++
			// The activity returns after its own run window, which the real
			// one does after an hour.
			s.Equal(time.Hour, in.MaxRun)
			s.Equal(30*time.Second, in.Interval)

			if calls >= 3 {
				// Enough to prove the loop repeats; the test would otherwise
				// run forever, because the watcher is designed never to stop.
				s.env.CancelWorkflow()
			}
			return nil
		})

	s.env.ExecuteWorkflow(DownloadWatcherWorkflow, watcherInput())

	s.True(s.env.IsWorkflowCompleted())
	s.GreaterOrEqual(calls, 3, "the watcher did not keep running")
}

// TestWatcherSurvivesAFailedActivity — a failed invocation is usually the
// database being unreachable, and the watcher has to keep trying rather than
// give up. Giving up would leave every request waiting on a torrent that
// finished hours ago.
func (s *WatcherSuite) TestWatcherSurvivesAFailedActivity() {
	var calls int
	s.env.OnActivity("WatchDownloads", mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ WatchInput) error {
			calls++
			if calls <= 2 {
				return errSimulated("the database is unreachable")
			}
			s.env.CancelWorkflow()
			return nil
		})

	s.env.ExecuteWorkflow(DownloadWatcherWorkflow, watcherInput())

	s.True(s.env.IsWorkflowCompleted())
	s.GreaterOrEqual(calls, 3, "the watcher gave up after a failure")
}

// TestWatcherStopsOnCancellation — cancellation comes from the deployment, and
// it has to end the watcher rather than being swallowed as an activity error.
func (s *WatcherSuite) TestWatcherStopsOnCancellation() {
	s.env.OnActivity("WatchDownloads", mock.Anything, mock.Anything).
		Return(nil)

	s.env.RegisterDelayedCallback(func() {
		s.env.CancelWorkflow()
	}, 5*time.Minute)

	s.env.ExecuteWorkflow(DownloadWatcherWorkflow, watcherInput())

	s.True(s.env.IsWorkflowCompleted())

	// A cancelled watcher returns without an error: there is no request
	// outcome to report and nothing to compensate for.
	s.NoError(s.env.GetWorkflowError())
}

// ---------------------------------------------------------------------------
// What the request workflow concludes from the watcher's row
// ---------------------------------------------------------------------------

// TestCheckDownloadInterpretation pins the mapping from what the watcher
// recorded to what the request workflow does about it.
//
// Those two are deliberately different things. The watcher writes facts — how
// far along, what the client says, when it stopped moving — and never decides
// anything. Every decision is here, in one table, which is what makes the
// behaviour reviewable without reading the activity.
func TestCheckDownloadInterpretation(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		descr     string
		progress  storage.DownloadProgress
		want      DownloadState
		blacklist bool
	}{
		{
			descr:    "still downloading",
			progress: storage.DownloadProgress{Progress: 0.4, QbitState: "downloading"},
			want:     DownloadRunning,
		},
		{
			descr:    "finished",
			progress: storage.DownloadProgress{Progress: 1.0, QbitState: "uploading"},
			want:     DownloadDone,
		},
		{
			descr:    "the client reports an error",
			progress: storage.DownloadProgress{Progress: 0.8, QbitState: "error"},
			want:     DownloadFailed,
			// The release failed for a reason that will repeat.
			blacklist: true,
		},
		{
			descr:     "the client cannot find the files",
			progress:  storage.DownloadProgress{Progress: 0.1, QbitState: "missingfiles"},
			want:      DownloadFailed,
			blacklist: true,
		},
		{
			descr: "stalled past the window",
			progress: storage.DownloadProgress{
				Progress:  0.3,
				QbitState: "stalledDL",
				ZeroSince: ptr(now.Add(-time.Hour)),
			},
			want:      DownloadStalled,
			blacklist: true,
		},
		{
			descr: "stopped past the window",
			progress: storage.DownloadProgress{
				Progress:  0.986,
				QbitState: "stoppedDL",
				ZeroSince: ptr(now.Add(-time.Hour)),
			},
			// The case found in a real library: a stopped download is a state
			// nobody asked for, and it must not be waited on for two days.
			want:      DownloadStalled,
			blacklist: true,
		},
		{
			descr: "stalled, but not for long enough yet",
			progress: storage.DownloadProgress{
				Progress:  0.3,
				QbitState: "stalledDL",
				ZeroSince: ptr(now.Add(-time.Minute)),
			},
			want: DownloadRunning,
		},
		{
			descr: "vanished after making progress",
			progress: storage.DownloadProgress{
				Progress:     0.4,
				QbitState:    "missing",
				MissingSince: ptr(now.Add(-10 * time.Minute)),
			},
			want: DownloadMissing,
			// It got partway and died, so the release itself is dead.
			blacklist: true,
		},
		{
			descr: "vanished without ever starting",
			progress: storage.DownloadProgress{
				Progress:     0,
				QbitState:    "missing",
				MissingSince: ptr(now.Add(-10 * time.Minute)),
			},
			want: DownloadMissing,
			// Never started, so this is the infrastructure's fault, and
			// blacklisting would make a client restart permanently poison
			// releases that are perfectly good.
			blacklist: false,
		},
		{
			descr: "vanished, but not for long enough yet",
			progress: storage.DownloadProgress{
				Progress:     0.4,
				QbitState:    "missing",
				MissingSince: ptr(now.Add(-30 * time.Second)),
			},
			// A single miss proves nothing: an add is asynchronous, and a
			// client that is restarting reports nothing for a moment.
			want: DownloadRunning,
		},
		{
			descr:    "the request already ended",
			progress: storage.DownloadProgress{State: storage.StateCompleted, Progress: 1.0},
			want:     DownloadDone,
		},
	} {
		t.Run(tc.descr, func(t *testing.T) {
			got := interpret(tc.progress, now)

			s := ""
			if got.Blacklist {
				s = " (blacklisted)"
			}
			if got.State != tc.want {
				t.Errorf("%s: state = %v, want %v%s", tc.descr, got.State, tc.want, s)
			}
			if got.Blacklist != tc.blacklist {
				t.Errorf("%s: Blacklist = %v, want %v", tc.descr, got.Blacklist, tc.blacklist)
			}
		})
	}
}

// TestWatcherTimestampsAreNotRewritten — carriedSince is what makes the
// timestamps mean "since when" rather than "as of now". Rewriting them on every
// poll would restart a stalled download's clock every thirty seconds, and the
// stall would never grow old enough to act on.
func TestWatcherTimestampsAreNotRewritten(t *testing.T) {
	earlier := time.Now().Add(-20 * time.Minute)
	now := time.Now()

	if got := carriedSince(&earlier, now); got == nil || !got.Equal(earlier) {
		t.Errorf("an existing timestamp was rewritten: got %v, want %v", got, earlier)
	}

	got := carriedSince(nil, now)
	if got == nil || !got.Equal(now) {
		t.Errorf("a missing timestamp was not set: got %v, want %v", got, now)
	}
}
