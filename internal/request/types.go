// Package request holds the workflow that fetches one piece of media.
//
// One workflow is one film or one season of a series. There is no "the whole
// series" request by design: a flat model keeps the workflow, the
// deduplication key and the UI card all describing the same thing.
//
// The workflow contains orchestration and nothing else. Scoring, parsing and
// delay computation live in activities or in the packages they call, because
// changing any of that inside a workflow would break the replay of executions
// that are already running — and these run for two years.
package request

import (
	"fmt"
	"time"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
)

// Workflow names and identifiers that form the contract with the event
// history.
//
// They are pinned constants rather than the Go function names, because the
// default type name is the function name: renaming OrderWorkflow would
// otherwise break every execution already in flight, and these last two years.
const (
	WorkflowType = "RequestWorkflow"
	// DownloadWatcherWorkflowType is the singleton that polls the torrent
	// client. Its name is pinned for the same reason as the other: a rename
	// would break the execution already running under the old one.
	DownloadWatcherWorkflowType = "DownloadWatcherWorkflow"

	// QueryState reports the workflow's own view of a request. It is answered
	// from workflow state and cannot change it.
	QueryState = "state"

	// SignalPick wakes a sleeping request because the user chose a release
	// from the candidate list.
	//
	// The signal carries no payload: the choice itself lives in the
	// database, written by the web UI or the bot before the signal is
	// sent. The signal is only the knock on the door — without it a
	// request that found nothing would sit out the rest of its backoff
	// ladder, an hour and then a day and then a day again, before it
	// noticed a pick made minutes after the search that produced the list.
	SignalPick = "pick"
)

// Policy is the retry and timing policy a workflow runs under.
//
// It travels in the input rather than being read from configuration inside the
// workflow. A workflow that reaches for a config file is not deterministic
// across a replay where that file has changed, and a two-year execution will
// outlive many edits to it. Freezing the policy at start also means a running
// request keeps behaving the way it did when it was created.
type Policy struct {
	// SearchDeadline bounds how long the request keeps looking. There is
	// deliberately no attempt limit: over two years any fixed count is reached
	// and becomes the real deadline.
	SearchDeadline time.Duration

	// BackoffLadder is how long to wait before each search, indexed by the
	// number of searches that have already found nothing. The last entry is
	// the ceiling.
	//
	// A strict hourly schedule would be 17 520 searches per request over two
	// years, each fanning out across every configured tracker. The ladder
	// gives roughly 735 for practically the same result: releases do not
	// appear in the first hour, and one that appears in a year is found within
	// a day.
	BackoffLadder []time.Duration

	// Jitter is added to every scheduled run.
	//
	// Without it, requests created in one batch stay aligned forever and every
	// hour becomes a spike against the indexer.
	Jitter time.Duration

	// PollInterval is how often the download loop wakes up.
	//
	// It is far longer than the interval at which progress is written to the
	// database, and that gap is deliberate: the user interface needs progress
	// every thirty seconds, while the workflow only needs to know
	// done/stalled/expired. Polling the workflow at thirty seconds for a
	// two-day download would cost about fifty thousand history events, which
	// is the termination limit.
	PollInterval time.Duration

	// ContinueAsNewAt is the history length at which the workflow restarts
	// itself. The server warns at 10 240 events and terminates at 51 200, and
	// termination is a lost request rather than an error.
	ContinueAsNewAt int

	// DownloadDeadline bounds one torrent attempt. A release that has not
	// finished in two days is treated as dead.
	DownloadDeadline time.Duration

	// ZeroStallAfter is how long a download may go without progress before the
	// release is abandoned. It is a duration rather than a poll count so that
	// it does not depend on the poll cadence or survive a restart differently.
	ZeroStallAfter time.Duration

	// MissingGrace is how long a torrent may be absent from the client before
	// that counts as gone.
	MissingGrace time.Duration

	// MaxStalls is how many releases may be abandoned before the request
	// fails. Three dead releases in a row means the content is not actually
	// available, and the user should be told rather than quietly retried
	// forever.
	MaxStalls int
}

// Input starts a request workflow.
type Input struct {
	// RequestID is the projection row this workflow owns.
	RequestID int64
	// Spec is the frozen description of the media, captured when the request
	// was created. Re-reading it from TMDB on every search would make the loop
	// depend on a service that can change underneath it.
	Spec domain.MediaSpec

	Policy Policy

	// The Start fields carry state across ContinueAsNew. A restart passes
	// arguments and nothing else, so anything not here is lost when the
	// history is reset.
	//
	// They also let a workflow be restarted deliberately — after a manual
	// retry, or by an operator resuming a failed request — without needing a
	// separate entry point.
	StartPhase       storage.State
	StartAttempt     int
	StartErrorStreak int
	StartStallCount  int
	StartNextRunAt   time.Time
	// StartIteration carries the loop count across a restart. It is only used
	// for logging, but a counter that silently resets is worse than none.
	StartIteration int
	// StartSelected and StartQbitHash carry a download that was in flight when
	// the history was reset. Losing them would leave the workflow unable to
	// clean up a torrent it had already added.
	StartSelected *domain.Release
	StartQbitHash string
	// StartSelectedThreadID, StartSelectedVideoCount and
	// StartSelectedEpisodeCount carry the three ongoing-season fields
	// across a restart. Dropping them would have the next run forget
	// which thread the previous pick came from, and the same
	// replacement-the-inflight-torrent trap follows.
	StartSelectedThreadID     string
	StartSelectedVideoCount   int
	StartSelectedEpisodeCount int
	// StartDownloadDeadline carries the current attempt's deadline, so a
	// restart does not hand the download a fresh two days.
	StartDownloadDeadline time.Time
	// StartSearchDeadline carries the end of the search.
	//
	// It is carried for the same reason as the download deadline, and the
	// consequence of dropping it is worse. A ContinueAsNew that recomputed the
	// window would give the request a fresh two years on every history reset,
	// so the product's deadline would never actually arrive: the only thing
	// left bounding the request would be WorkflowExecutionTimeout, which
	// *terminates* the execution — no result, no notification, and a
	// projection row that stays "searching" forever.
	StartSearchDeadline time.Time
	// StartManualSince carries the moment the workflow entered StateManual.
	// A ContinueAsNew during manual mode would otherwise reset the 14-day
	// manual-mode timeout on every history reset, and the same trap as
	// StartSearchDeadline (the timeout never arrives) follows.
	StartManualSince time.Time
}

// State is the workflow's own state, carried across ContinueAsNew.
//
// It is explicit rather than implicit in local variables, because
// ContinueAsNew passes arguments and nothing else: anything not in here is
// lost when the history is reset.
type State struct {
	Phase storage.State

	Attempt     int
	ErrorStreak int
	StallCount  int

	// Iteration counts every pass of the outer loop. It is what decides when
	// to restart the history, and it is carried across so that the count
	// survives.
	Iteration int

	NextRunAt time.Time

	// Selected is the release currently being downloaded, if any.
	Selected *domain.Release
	// QbitHash is the authoritative hash, as reported by the client.
	QbitHash string
	// SelectedThreadID is the forum thread id of the selected release,
	// captured by the indexer. Set whenever a release with a non-empty
	// ThreadID is chosen, regardless of whether the season turned out
	// complete or partial. The next search tick reads it to prefer a
	// release from the same thread over a fresh infohash from a
	// different one.
	SelectedThreadID string
	// SelectedVideoCount is the count of video files the qBittorrent
	// client reported when the torrent was added. Compared against
	// SelectedEpisodeCount to drive the full-or-ongoing decision.
	SelectedVideoCount int
	// SelectedEpisodeCount is the season's full episode count from
	// TMDB, captured at selection time.
	SelectedEpisodeCount int
	// DownloadDeadline is when the current torrent attempt is abandoned.
	DownloadDeadline time.Time

	// SearchDeadline is the absolute end of the search, computed once so that
	// it survives every restart.
	SearchDeadline time.Time

	// ManualSince is set when the workflow enters StateManual (waiting for
	// the user to pick a release) and cleared when it leaves. The 14-day
	// manual-mode timeout is "now - ManualSince > 14d", and the field has
	// to persist across ContinueAsNew the way the other deadlines do.
	ManualSince time.Time
}

// Result is what the workflow returns.
//
// A cancelled request returns a normal result describing what happened rather
// than an error. That is deliberate: the caller gets something it can show,
// and every terminal outcome of this workflow — completed, skipped, failed,
// cancelled — is a successful evaluation that reached a conclusion. Only a
// bug or an unrecoverable infrastructure failure is an error.
type Result struct {
	RequestID int64
	Outcome   storage.State

	Title    string
	Year     int
	Reason   string
	Attempts int

	// Selected is the release that was downloaded, when there was one.
	Selected *domain.Release
	// PlexTitle is what matched in the library, for a skip. It is surfaced so
	// that a wrong match is visible to the user rather than silently
	// swallowing their request.
	PlexTitle string
	PlexYear  int
}

// String renders the result for logs.
func (r Result) String() string {
	s := fmt.Sprintf("%s %s after %d searches", r.Outcome, r.Title, r.Attempts)
	if r.Reason != "" {
		s += ": " + r.Reason
	}
	return s
}

// ---------------------------------------------------------------------------
// Activity inputs and outputs
// ---------------------------------------------------------------------------

// CheckPlexInput asks whether the library already has the media.
type CheckPlexInput struct {
	// RequestID is how the activity finds the media row; it does not need the
	// caller to know the media id.
	RequestID int64
	Spec      domain.MediaSpec
	// Force re-asks even when the cached answer is recent.
	Force bool
	// MaxAge is how stale the cached answer may be before it is re-checked.
	MaxAge time.Duration
}

// CheckPlexResult is the answer.
type CheckPlexResult struct {
	InPlex bool
	// Title and Year are what matched, so the user can see what the system
	// thought it found.
	Title string
	Year  int
	// Checked reports whether the answer came from a fresh query. False means
	// the cache was used.
	Checked bool
}

// SearchInput asks the indexer for releases.
type SearchInput struct {
	RequestID int64
	Spec      domain.MediaSpec
	// MaxAge marks how long the caller is willing to wait for a torrent
	// client that is slow to list what it added.
	Limit int
	// OngoingThread is the forum thread id of the previously-selected
	// release, when the request is in StateOngoing. Empty on a fresh
	// request; the selector uses it to prefer a release from the same
	// thread over a fresh infohash from a different one.
	OngoingThread string
}

// SearchResult is the outcome of one search.
type SearchResult struct {
	// Chosen is the release to download. Nil means nothing acceptable was
	// found, which is an ordinary outcome rather than an error.
	Chosen *domain.Release

	// Considered and Rejected describe what happened, so that "there were
	// results, why did nothing download" is answerable from the event log
	// without reproducing the search.
	Considered int
	Summary    string
	Rejections []Rejection

	// Candidates is how many releases the user can pick from: the ones the
	// selector refused, with the reason. The list itself is persisted by
	// the search activity and never travels through the workflow's event
	// history — a hundred releases with a magnet URI each would bloat
	// every fruitless search of every request.
	Candidates int

	// CandidatesNew reports that this search is the one that put the list
	// there. The workflow announces the list once, when it first appears:
	// the list is refreshed on every fruitless search, and an announcement
	// per search would be a notification per hour for two years.
	CandidatesNew bool
}

// Found reports whether a release was selected.
func (r SearchResult) Found() bool { return r.Chosen != nil }

// Rejection mirrors the selector's reason codes across the activity boundary.
//
// Only the code and a title travel: sending whole releases through the event
// history for releases that were refused would bloat it for no benefit, since
// the point is to answer "why", not to keep the rejected candidates.
type Rejection struct {
	Title  string
	Reason string
	Detail string
}

// AddTorrentInput hands a release to the torrent client.
type AddTorrentInput struct {
	RequestID int64
	// Spec is the media the release was selected for, carried through so
	// AddTorrent can pick the per-type save path without a second lookup.
	Spec    domain.MediaSpec
	Release domain.Release
}

// AddTorrentResult reports what the client did.
type AddTorrentResult struct {
	Hash string
	// AlreadyPresent means the client already had this torrent. Temporal
	// retries activities after a worker crash, so this is a normal outcome
	// rather than a problem.
	AlreadyPresent bool
}

// CheckDownloadInput asks how the current torrent is doing.
type CheckDownloadInput struct {
	RequestID int64
	// StallAfter is how long a download may go without progress before the
	// release is abandoned.
	StallAfter time.Duration
	// MissingGrace is how long a torrent may be absent from the client before
	// that counts. A single miss proves nothing: an add is asynchronous, and a
	// client that is restarting reports nothing for a moment.
	MissingGrace time.Duration
}

// DownloadState is the workflow's view of a download.
type DownloadState string

const (
	DownloadRunning DownloadState = "downloading"
	DownloadDone    DownloadState = "done"
	DownloadStalled DownloadState = "stalled"
	DownloadMissing DownloadState = "missing"
	DownloadFailed  DownloadState = "failed"
)

// CheckDownloadResult is the answer.
//
// It carries no progress numbers. The workflow does not act on them — the
// interface reads those from the projection — and passing them through would
// put a history event's worth of payload on every poll for no decision.
type CheckDownloadResult struct {
	State DownloadState
	// Detail explains a stalled, missing or failed outcome.
	Detail string
	// Hash is the authoritative client hash, which can differ from the
	// indexer's for hybrid torrents and is learned on the first poll.
	Hash string
	// ContentPath is where the client put the download, which the Plex refresh
	// needs. It rides along with the poll rather than being fetched by an
	// activity of its own: this call already reads the row it lives in.
	ContentPath string
	// Blacklist reports whether the release should be remembered as unusable.
	//
	// The decision is made where the evidence is and passed to the workflow
	// rather than re-derived there, because the reason differs by case: a
	// torrent that errored is always the release's fault, a stalled one
	// usually is, and one that vanished without ever starting is the
	// infrastructure's.
	Blacklist bool
}

// DeleteTorrentInput removes a torrent as compensation.
type DeleteTorrentInput struct {
	RequestID int64
	Hash      string
	// DeleteFiles must be chosen deliberately: it is safe when the client's
	// save path is outside the Plex library and destructive when it is inside.
	DeleteFiles bool
	// BlacklistFor and Reason record the release as unusable in the same call,
	// so that abandoning a bad release cannot half-happen: a torrent removed
	// without being blacklisted would be picked again on the next search.
	BlacklistFor string
	Reason       string
}

// RefreshPlexInput asks the server to rescan one directory.
type RefreshPlexInput struct {
	Spec domain.MediaSpec
	Path string
}

// VerifyPlexInput asks whether the media showed up after a download.
type VerifyPlexInput struct {
	MediaID int64
	Spec    domain.MediaSpec
}

// NotifyInput tells the watchers what happened.
type NotifyInput struct {
	RequestID int64
	Kind      storage.NotificationKind
	Title     string
	Body      string
	Payload   any
}

// SaveStateInput writes the workflow's view of a request to the projection.
type SaveStateInput struct {
	RequestID int64
	State     storage.RequestState
	// Event is recorded alongside the state change in the same transaction, so
	// the audit trail cannot drift from the state it describes.
	Event string
	Actor string
	// Detail is stored as JSON on the event.
	Detail any
}

// VideoFileCountInput asks the torrent client how many video files
// the just-added torrent contains. Used by the ongoing-season
// branch to decide whether the picked release is the full season
// or a partial one.
type VideoFileCountInput struct {
	// Hash is the qBittorrent infohash returned by AddTorrent.
	Hash string
}

// VideoFileCountResult is the count of video files in the torrent.
// Subtitles, samples and other non-video files are not counted;
// see qbit.isVideoFile for the exact list.
type VideoFileCountResult struct {
	Count int
}

// SeasonEpisodeCountInput asks TMDB how many episodes a season
// has. Empty EpisodeCount (zero) means the season is not yet
// known — TMDB returns that for a still-airing season, and the
// workflow then skips the ongoing branch.
type SeasonEpisodeCountInput struct {
	TMDBID int64
	Season int
}

// SeasonEpisodeCountResult is the count TMDB reports.
type SeasonEpisodeCountResult struct {
	Count int
}
