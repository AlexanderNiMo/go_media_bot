package request

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/temporalx"
)

// Workflow fetches one piece of media: it checks whether Plex already has it
// and, if not, keeps looking for a release until one is found or the search
// deadline passes.
//
// Cancellation is Temporal's own, observed through the workflow context. That
// is why the waits below race a timer against ctx.Done() rather than calling
// workflow.Sleep: a sleep cannot be interrupted, so a request parked in a
// daily backoff would take a day to notice that the user changed their mind.
func Workflow(ctx workflow.Context, in Input) (Result, error) {
	state := initialState(ctx, in)

	// Registered before anything else, and that is not stylistic. A query
	// handler is not written to history, so after a worker restart the server
	// answers a query by replaying the workflow — and the replay only reaches
	// a handler that sits on its path.
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (State, error) {
		return state, nil
	}); err != nil {
		return Result{}, err
	}

	workflow.GetLogger(ctx).Info("request workflow started",
		"request", in.RequestID, "media", in.Spec.String(), "phase", state.Phase)

	// A workflow that was restarted mid-download resumes the download rather
	// than starting a second one. This is the path that ContinueAsNew takes
	// when it happens while a torrent is in flight.
	if state.QbitHash != "" && state.Phase == storage.StateDownloading {
		result, err := pollDownload(ctx, &in, &state)
		if result != nil || err != nil {
			if err != nil {
				return Result{}, err
			}
			return *result, nil
		}
	}

	// The Plex check comes first, before any searching. Asking the library
	// costs one request; searching costs a fan-out across every tracker.
	plex, err := checkPlex(ctx, in, false)
	if err != nil {
		if cancellationIsDue(ctx, err) {
			result, cancelErr := cancelRequest(ctx, &in, &state)
			if cancelErr != nil {
				return Result{}, cancelErr
			}
			return *result, nil
		}
		return Result{}, err
	}
	if plex.InPlex {
		return finishSkipped(ctx, in, &state, plex)
	}

	for {
		// Restarting the history is checked on every iteration rather than on
		// a counter, because the cost of an iteration is easy to underestimate
		// and the server terminates a workflow that exceeds its limit.
		if done, err := continueAsNewIfNeeded(ctx, in, state); done || err != nil {
			return Result{}, err
		}

		result, err := runSearchCycle(ctx, &in, &state)
		if err != nil {
			if cancellationIsDue(ctx, err) {
				cancelled, cancelErr := cancelRequest(ctx, &in, &state)
				if cancelErr != nil {
					return Result{}, cancelErr
				}
				return *cancelled, nil
			}
			return Result{}, err
		}
		if result != nil {
			return *result, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Search cycle
// ---------------------------------------------------------------------------

// runSearchCycle performs one pass: wait, search, select, add, download.
//
// It returns a non-nil result when the request has reached a terminal state,
// and nil to ask the caller to loop again.
func runSearchCycle(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	state.Iteration++

	// Waiting for the next attempt is the common case: most passes of this
	// loop, for most of a two-year request, find nothing and go back to sleep.
	cancelled, err := waitUntil(ctx, state.NextRunAt)
	if err != nil {
		return nil, err
	}
	if cancelled {
		return cancelRequest(ctx, in, state)
	}

	if workflow.Now(ctx).After(state.SearchDeadline) {
		return failedResult(ctx, in, state,
			"the search deadline passed without finding an acceptable release")
	}

	outcome, err := searchOnce(ctx, in, state)
	if err != nil {
		// An activity that was in flight when the cancellation landed is
		// itself cancelled, and its error arrives looking like a failure. It
		// has to be recognised as a cancellation here, or the user who
		// cancelled gets an error instead of a clean outcome.
		if cancellationIsDue(ctx, err) {
			return cancelRequest(ctx, in, state)
		}
		return nil, err
	}

	switch outcome {
	case searchContinue:
		return nil, nil
	case searchSatisfied:
		return skippedResult(ctx, in, state)
	case searchFound:
		return downloadCycle(ctx, in, state)
	case searchManual:
		return manualPhase(ctx, in, state)
	default:
		// Unreachable, but a workflow that silently does nothing is worse than
		// one that says so.
		return nil, fmt.Errorf("unknown search outcome %d", outcome)
	}
}

// searchOutcome is what one search concluded.
type searchOutcome int

const (
	// searchContinue means nothing acceptable was found and the next attempt
	// has been scheduled. It is the ordinary result of most searches.
	searchContinue searchOutcome = iota
	// searchFound means a release was selected and is ready to be added.
	searchFound
	// searchSatisfied means the media turned up in Plex between the search and
	// the selection, so there is nothing to download.
	searchSatisfied
	// searchManual means the auto-selector found nothing acceptable but the
	// indexer did return releases, and the user has been given a list to
	// pick from. The caller is expected to enter StateManual and wait for
	// a pick or the 14-day manual-mode timeout.
	searchManual
)

// searchOnce runs one search and selection, updating the schedule.
func searchOnce(ctx workflow.Context, in *Input, state *State) (searchOutcome, error) {
	actCtx := withOptions(ctx, indexerOptions())

	// A user-submitted magnet bypasses the indexer. The link is consumed
	// here, the workflow falls through to the add path with a synthetic
	// Release that has no infohash (qBittorrent fills it in).
	var link string
	if err := workflow.ExecuteActivity(actCtx, (*Activities).PopManualLink, in.RequestID).
		Get(actCtx, &link); err != nil {
		return searchContinue, fmt.Errorf("pop manual link: %w", err)
	}
	if link != "" {
		// Build a Release with the bare minimum the AddTorrent path
		// needs: a Link and a Title for the audit trail. Other fields
		// are zero — the score-based selection has nothing to choose
		// from, and the scoring/identity helpers are only used by the
		// selector, which is bypassed here.
		state.Selected = manualReleaseFromLink(link, in.Spec.DisplayTitle())
		state.Phase = storage.StateAdding
		if err := saveState(ctx, in, state, "release.manual", "user-submitted link"); err != nil {
			return searchContinue, err
		}
		return searchFound, nil
	}

	var res SearchResult
	err := workflow.ExecuteActivity(actCtx, (*Activities).SearchReleases, SearchInput{
		RequestID: in.RequestID,
		Spec:      in.Spec,
		Limit:     100,
	}).Get(actCtx, &res)
	if err != nil {
		// An indexer failure is our problem, not the media's, so the attempt
		// counter is not advanced and the backoff is driven by a separate
		// streak. Burning a search on a tracker outage would eventually fail a
		// request that was going to succeed on its own.
		state.ErrorStreak++
		state.Phase = storage.StatePending
		state.NextRunAt = workflow.Now(ctx).Add(indexerBackoff(state.ErrorStreak))

		reason := describeError(err, "indexer error")
		if saveErr := saveState(ctx, in, state, "search.indexer_error", reason); saveErr != nil {
			return searchContinue, saveErr
		}
		workflow.GetLogger(ctx).Warn("indexer search failed",
			"request", in.RequestID, "streak", state.ErrorStreak, "error", reason)
		return searchContinue, nil
	}

	state.ErrorStreak = 0

	if !res.Found() {
		// For media old enough that auto-search is unlikely to find
		// anything (the indexer has dropped the release, the trackers
		// have aged out, russian_audio: missing rules out otherwise-good
		// candidates), the user gets a list to pick from. Newer media
		// just goes back to sleep on the normal backoff — the auto-pick
		// is more useful there because there is likely something
		// acceptable coming in the next refresh.
		if outcome, err := maybeEnterManualMode(ctx, in, state); err != nil {
			return searchContinue, err
		} else if outcome == searchManual {
			return searchManual, nil
		}
		return searchContinue, scheduleNextSearch(ctx, in, state, res)
	}

	// Found something. Before doing anything irreversible, ask Plex once more:
	// the most common way to end up with a duplicate is another tool grabbing
	// the same thing while this request was waiting.
	plex, err := checkPlex(ctx, *in, true)
	if err != nil {
		return searchContinue, err
	}
	if plex.InPlex {
		state.Phase = storage.StateSkipped
		if err := saveState(ctx, in, state, "plex.appeared", plex.Title); err != nil {
			return searchContinue, err
		}
		return searchSatisfied, nil
	}

	state.Selected = res.Chosen
	state.Phase = storage.StateAdding
	if err := saveState(ctx, in, state, "release.selected", res.Summary); err != nil {
		return searchContinue, err
	}

	return searchFound, nil
}

// scheduleNextSearch sets when to look again after a fruitless search.
func scheduleNextSearch(ctx workflow.Context, in *Input, state *State, res SearchResult) error {
	now := workflow.Now(ctx)

	// A film that has not been released yet is not a failed search. The
	// attempt counter is rolled back and the next run is pushed out to around
	// the release date, which is the difference between a request that quietly
	// waits and one that burns seven hundred pointless searches before failing
	// on a film that never came out.
	if next, deferred := notYetReleased(in.Spec, now); deferred {
		state.Phase = storage.StatePending
		state.NextRunAt = next
		return saveState(ctx, in, state, "search.not_yet_released",
			"the film has not been released yet")
	}

	state.Attempt++
	state.Phase = storage.StatePending
	state.NextRunAt = now.Add(backoffFor(in.Policy, state.Attempt))

	return saveState(ctx, in, state, "search.no_acceptable", res.Summary)
}

// isOldMedia reports whether the spec's year is more than two years behind
// now. The cutoff is intentionally generous: anything from 2023 or
// earlier in 2026 counts as old. A request for a 2024 film does not
// get a manual list yet — the indexer is more likely to surface a
// good release on the next refresh, and the user would be pestered
// with a list every backoff tick.
//
// Year=0 (unknown) returns false. The selector's russian_audio rule
// already filters out most "old enough to need a manual pick" releases
// on its own, so the unknown-year case is mostly a self-search
// where no manual list is wanted either.
func isOldMedia(spec domain.MediaSpec, now time.Time) bool {
	if spec.Year <= 0 {
		return false
	}
	return spec.Year <= now.Year()-2
}

// maybeEnterManualMode is the searchOnce fallback for media old enough
// that the indexer usually returns nothing. It runs the indexer one
// more time via FetchCandidates, persists the rejected list, and tells
// the caller to enter StateManual if there is anything for the user
// to look at.
//
// Three outcomes:
//   - searchManual: a list was persisted, the workflow should now
//     run manualPhase, the user has been notified.
//   - searchContinue: the indexer returned nothing, or the media is
//     not old enough to qualify. The caller falls through to the
//     normal backoff.
//   - error: an activity or storage write failed; the caller
//     continues on the normal backoff rather than losing a search
//     cycle to a transient failure.
func maybeEnterManualMode(ctx workflow.Context, in *Input, state *State) (searchOutcome, error) {
	now := workflow.Now(ctx)
	if !isOldMedia(in.Spec, now) {
		return searchContinue, nil
	}

	// Already in manual mode? Don't re-fetch and re-notify on every search
	// tick — the user is given the list once and asked to pick or wait
	// 14 days. The main loop is responsible for the timeout.
	if state.Phase == storage.StateManual {
		return searchManual, nil
	}

	actCtx := withOptions(ctx, indexerOptions())
	var candidates []storage.Candidate
	if err := workflow.ExecuteActivity(actCtx, (*Activities).FetchCandidates, SearchInput{
		RequestID: in.RequestID,
		Spec:      in.Spec,
		Limit:     100,
	}).Get(actCtx, &candidates); err != nil {
		// A search error is not a manual-mode error: the indexer
		// might be flaky, and the workflow's backoff already covers
		// that. Surface the error so the caller falls through to
		// scheduleNextSearch, which will reschedule.
		workflow.GetLogger(ctx).Warn("could not fetch candidates for manual pick", "error", err)
		return searchContinue, err
	}
	if len(candidates) == 0 {
		// Indexer really did return nothing this time; the manual
		// list would be empty and offering it is noise. Backoff.
		return searchContinue, nil
	}

	if err := workflow.ExecuteActivity(actCtx, (*Activities).StoreCandidates, StoreCandidatesInput{
		RequestID:  in.RequestID,
		Candidates: candidates,
	}).Get(actCtx, nil); err != nil {
		return searchContinue, err
	}

	state.Phase = storage.StateManual
	state.ManualSince = now
	// First poll: one minute from now. Subsequent polls use the same
	// cadence (manualPhase bumps it back to 1 minute before sleeping).
	state.NextRunAt = now.Add(time.Minute)
	if err := saveState(ctx, in, state, "search.manual_candidates",
		fmt.Sprintf("offered %d candidates for manual pick", len(candidates))); err != nil {
		return searchContinue, err
	}

	if err := notify(ctx, in, storage.NotificationManual,
		fmt.Sprintf("Pick a release: %s", in.Spec.DisplayTitle()),
		buildCandidateList(candidates)); err != nil {
		// A notification failure must not lose the manual entry. The
		// list is in the DB; the user will see it on the request page
		// even if Telegram did not deliver.
		workflow.GetLogger(ctx).Warn("manual-mode notification failed", "error", err)
	}

	workflow.GetLogger(ctx).Info("entered manual-pick mode",
		"request", in.RequestID, "candidates", len(candidates))
	return searchManual, nil
}

// buildCandidateList renders the candidate list for a manual-mode
// notification body. Plain text — the bot splits on newlines.
func buildCandidateList(candidates []storage.Candidate) string {
	var b strings.Builder
	for _, c := range candidates {
		fmt.Fprintf(&b, "%d. [%s] %s\n   seeders: %d  size: %s  rejected: %s\n",
			c.Rank, c.Indexer, c.Title, c.Seeders,
			humanSize(c.SizeBytes), c.RejectionReason)
	}
	return b.String()
}

// humanSize formats a byte count for the notification body.
func humanSize(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	if n < k*k {
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	}
	if n < k*k*k {
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
}

// manualPhase runs while the request is in StateManual: polls
// GetPickedCandidate on every tick, exits with the picked release
// as a result (downloadCycle takes over from there) or with nil
// when the 14-day manual-mode timeout fires.
//
// It returns a non-nil *Result only when the user has picked; the
// download cycle then takes over via the caller's switch. On timeout
// it returns nil so the caller loops and re-enters the search cycle
// (state has been reset to StatePending before this returns).
func manualPhase(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	const manualTimeout = 14 * 24 * time.Hour
	actCtx := withOptions(ctx, dbOptions())

	for {
		now := workflow.Now(ctx)
		// Timeout: the user had 14 days. Clear the list, go back to
		// the normal search cycle. Reset Attempt so a fresh search
		// has a clean backoff ladder, and put NextRunAt in the
		// immediate future so the next pass is not a sleep.
		if !state.ManualSince.IsZero() && now.Sub(state.ManualSince) > manualTimeout {
			if err := workflow.ExecuteActivity(actCtx, (*Activities).ClearCandidates, in.RequestID).
				Get(actCtx, nil); err != nil {
				return nil, err
			}
			state.Phase = storage.StatePending
			state.Attempt = 0
			state.NextRunAt = now
			state.ManualSince = time.Time{}
			if err := saveState(ctx, in, state, "search.manual_timeout",
				"the user did not pick a release within 14 days"); err != nil {
				return nil, err
			}
			if err := notify(ctx, in, storage.NotificationFailed,
				"Manual pick timed out: "+in.Spec.DisplayTitle(),
				"No release was picked from the candidate list within 14 days. "+
					"The request is back to the normal search cycle."); err != nil {
				workflow.GetLogger(ctx).Warn("manual-timeout notification failed", "error", err)
			}
			return nil, nil
		}

		// Cancellation: a user who cancels the request while it is
		// sitting in manual mode wants the wait over, not a
		// notification about a download that is not going to
		// happen. Fall through to the same path the search cycle
		// uses; cancelRequest will see the state and clean up.
		if ctx.Err() != nil {
			return cancelRequest(ctx, in, state)
		}

		// The user picked. Build a release from the candidate data
		// and hand it to downloadCycle. The candidate list is
		// already cleared by the pick (the storage activity
		// MarkPicked only sets picked_at; ClearCandidates is called
		// below after the pick is consumed to be safe across
		// retries).
		var picked storage.Candidate
		err := workflow.ExecuteActivity(actCtx, (*Activities).GetPickedCandidate, in.RequestID).
			Get(actCtx, &picked)
		if err == nil {
			// Got a pick. Clear the list, build the release, go to
			// downloadCycle. If ClearCandidates fails, log and
			// continue — the download is the user's intent and a
			// stale candidate row is harmless.
			if clearErr := workflow.ExecuteActivity(actCtx, (*Activities).ClearCandidates, in.RequestID).
				Get(actCtx, nil); clearErr != nil {
				workflow.GetLogger(ctx).Warn("clear candidates after pick failed", "error", clearErr)
			}
			state.Selected = releaseFromCandidate(picked, in.Spec)
			state.Phase = storage.StateAdding
			state.ManualSince = time.Time{}
			if saveErr := saveState(ctx, in, state, "release.manual_picked",
				fmt.Sprintf("user picked rank %d (%s)", picked.Rank, picked.Title)); saveErr != nil {
				return nil, saveErr
			}
			workflow.GetLogger(ctx).Info("user picked a release",
				"request", in.RequestID, "rank", picked.Rank, "title", picked.Title)
			return downloadCycle(ctx, in, state)
		}
		// NoSuchCandidate is the activity's typed "not yet picked"
		// signal. It travels as a Temporal ApplicationError with
		// type "NoSuchCandidate"; the storage sentinel itself does
		// not survive the activity boundary. Anything else is a
		// real error and we should not silently swallow it.
		var appErr *temporal.ApplicationError
		if !(errors.As(err, &appErr) && appErr.Type() == "NoSuchCandidate") {
			return nil, err
		}

		// Wait until the next poll. Same shape as runSearchCycle's
		// waitUntil: cancellable, with a short cadence (one
		// minute) because the user is sitting at a screen waiting
		// to see their pick take effect.
		cancelled, err := waitUntil(ctx, state.NextRunAt)
		if err != nil {
			return nil, err
		}
		if cancelled {
			return cancelRequest(ctx, in, state)
		}
		state.NextRunAt = workflow.Now(ctx).Add(time.Minute)
		_ = state.NextRunAt // keep gofmt from complaining; the tick continues
	}
}

// releaseFromCandidate builds a domain.Release the rest of the
// pipeline (downloadCycle, AddTorrent) can consume. The candidate
// carries a magnet URI, so the existing manual-link path that the
// workflow already has for PopManualLink works the same way: the
// download cycle hands the link to qBittorrent.
func releaseFromCandidate(c storage.Candidate, spec domain.MediaSpec) *domain.Release {
	return &domain.Release{
		Title:       c.Title,
		InfoHash:    c.InfoHash,
		Indexer:     c.Indexer,
		MagnetURI:   c.MagnetLink,
		DownloadURL: c.SourceURL,
		// SizeBytes/Seeders/Leechers carry the candidate's data
		// so the audit trail in the projection row reflects the
		// release the user actually picked, not the indexer
		// result the selector would have picked if anything
		// had passed.
		SizeBytes: c.SizeBytes,
		Seeders:   c.Seeders,
		Leechers:  c.Leechers,
	}
}

// downloadCycle adds the selected release and waits for it to finish.
func downloadCycle(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	if state.Selected == nil {
		return nil, errors.New("download cycle entered with no selected release")
	}

	// Adding is the irreversible step, and it is why the release is persisted
	// before the call rather than after: a workflow cancelled or restarted
	// between the two still knows what it asked the client to do.
	actCtx := withOptions(ctx, qbitOptions())

	var added AddTorrentResult
	err := workflow.ExecuteActivity(actCtx, (*Activities).AddTorrent, AddTorrentInput{
		RequestID: in.RequestID,
		Spec:      in.Spec,
		Release:   *state.Selected,
	}).Get(actCtx, &added)
	if err != nil {
		if cancellationIsDue(ctx, err) {
			return cancelRequest(ctx, in, state)
		}
		// A disk-full error is terminal, not transient. Retrying would
		// either fail the same way or, worse, succeed and add a torrent
		// that nothing can read. Surface it as a request failure with
		// the operator's alert already in their inbox; do not blacklist
		// the release (the release is fine) and do not back off (the
		// backoff just hides the problem).
		if isNoDiskSpaceError(err) {
			result, ferr := failedResult(ctx, in, state,
				"qBittorrent is out of disk space; the operator has been notified")
			return result, ferr
		}
		if saveErr := handleAddFailure(ctx, in, state, err); saveErr != nil {
			return nil, saveErr
		}
		return nil, nil
	}

	state.QbitHash = added.Hash
	state.Phase = storage.StateDownloading
	state.DownloadDeadline = workflow.Now(ctx).Add(in.Policy.DownloadDeadline)

	if err := saveState(ctx, in, state, "torrent.added", added.Hash); err != nil {
		return nil, err
	}

	if err := notify(ctx, in, storage.NotificationStarted,
		fmt.Sprintf("Downloading %s", in.Spec.DisplayTitle()),
		describeRelease(*state.Selected)); err != nil {
		return nil, err
	}

	return pollDownload(ctx, in, state)
}

// pollDownload waits for the current torrent, one poll interval at a time.
//
// It returns nil to send the caller back to the search loop when the release
// was abandoned, which is not a terminal outcome: the request carries on
// looking for a different release.
func pollDownload(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	for {
		cancelled, err := waitUntil(ctx, workflow.Now(ctx).Add(in.Policy.PollInterval))
		if err != nil {
			return nil, err
		}
		if cancelled {
			return cancelRequest(ctx, in, state)
		}

		// The workflow never talks to the torrent client. It reads its own row,
		// which the download watcher keeps current — and that is what keeps a
		// two-day download from costing a history event per progress poll.
		actCtx := withOptions(ctx, dbOptions())

		var status CheckDownloadResult
		if err := workflow.ExecuteActivity(actCtx, (*Activities).CheckDownload, CheckDownloadInput{
			RequestID:    in.RequestID,
			StallAfter:   in.Policy.ZeroStallAfter,
			MissingGrace: in.Policy.MissingGrace,
		}).Get(actCtx, &status); err != nil {
			if cancellationIsDue(ctx, err) {
				return cancelRequest(ctx, in, state)
			}
			return nil, err
		}

		if status.Hash != "" && status.Hash != state.QbitHash {
			// The client's own hash is authoritative: for hybrid v1/v2
			// torrents it can differ from the one the indexer advertised.
			state.QbitHash = status.Hash
			if err := saveState(ctx, in, state, "torrent.hash_learned", status.Hash); err != nil {
				return nil, err
			}
		}

		// The attempt deadline is checked before the status, so that a torrent
		// which is still reporting progress but has been running for two days
		// is abandoned rather than waited on forever. A torrent that is alive
		// but never finishes is indistinguishable, from the poll loop's point
		// of view, from one that will finish any minute — and the request can
		// still find a different release.
		if !state.DownloadDeadline.IsZero() && workflow.Now(ctx).After(state.DownloadDeadline) {
			status = CheckDownloadResult{
				State: DownloadStalled,
				Hash:  state.QbitHash,
				Detail: fmt.Sprintf("the download did not finish within %s",
					in.Policy.DownloadDeadline),
				// Two days on one torrent without finishing means the release
				// is not going to arrive at a usable rate, whatever the client
				// says about its progress. That is the release's problem, so it
				// is remembered as bad.
				Blacklist: true,
			}
		}

		switch status.State {
		case DownloadDone:
			return completeDownload(ctx, in, state, status.ContentPath)

		case DownloadRunning:
			// Keep waiting. Progress is written by the download watcher.

		default:
			// Stalled, missing or failed. All three mean the same thing here:
			// abandon the release and look for another.
			result, err := abandonRelease(ctx, in, state, status)
			if err != nil {
				return nil, err
			}
			if result != nil {
				return result, nil
			}
			// Back to the search loop for a different release.
			return nil, nil
		}
	}
}

// abandonRelease blacklists a dead release and decides what happens next.
//
// It returns a non-nil result when the request is over, and nil to send the
// caller back to the search loop.
func abandonRelease(
	ctx workflow.Context,
	in *Input,
	state *State,
	status CheckDownloadResult,
) (*Result, error) {
	infohash := ""
	if state.Selected != nil {
		infohash = state.Selected.Identity()
	}

	workflow.GetLogger(ctx).Warn("abandoning release",
		"request", in.RequestID, "state", status.State, "detail", status.Detail,
		"release", infohash)

	// Remove the torrent and its files. A release the client never finished has
	// not been filed into the library, so this cannot destroy anything the user
	// is watching.
	//
	// Whether to blacklist comes from the activity rather than being decided
	// here, because the evidence is there: a torrent that errored is the
	// release's fault, a stalled one usually is, and one that vanished without
	// ever starting is the infrastructure's — and blacklisting that last case
	// would make a client restart permanently poison releases that are
	// perfectly good.
	blacklistFor := ""
	if status.Blacklist {
		blacklistFor = infohash
	}

	delCtx := withOptions(ctx, deleteOptions())
	_ = workflow.ExecuteActivity(delCtx, (*Activities).DeleteTorrent, DeleteTorrentInput{
		RequestID:    in.RequestID,
		Hash:         state.QbitHash,
		DeleteFiles:  true,
		BlacklistFor: blacklistFor,
		Reason:       string(status.State) + ": " + status.Detail,
	}).Get(delCtx, nil)

	state.StallCount++
	state.Selected = nil
	state.QbitHash = ""
	state.DownloadDeadline = time.Time{}

	if state.StallCount >= in.Policy.MaxStalls {
		return failedResult(ctx, in, state, fmt.Sprintf(
			"%d releases in a row failed to download; the content does not appear to be available",
			state.StallCount))
	}

	state.Phase = storage.StatePending
	state.NextRunAt = workflow.Now(ctx).Add(backoffLadderTop(in.Policy))

	if err := saveState(ctx, in, state, "download.abandoned", status.Detail); err != nil {
		return nil, err
	}
	return nil, nil
}

// completeDownload handles a finished torrent.
func completeDownload(ctx workflow.Context, in *Input, state *State, contentPath string) (*Result, error) {
	if err := saveState(ctx, in, state, "download.completed", ""); err != nil {
		return nil, err
	}

	// Ask Plex to rescan, so the media appears without the user triggering a
	// scan by hand.
	//
	// A failure here is not worth failing the request over: the download did
	// finish, and the server's own periodic scan will pick the file up
	// eventually. Losing the fact that it finished would be much worse.
	refreshPlex(ctx, in, contentPath)

	result := Result{
		RequestID: in.RequestID,
		Outcome:   storage.StateCompleted,
		Title:     in.Spec.DisplayTitle(),
		Year:      in.Spec.Year,
		// Surface what landed, not just that something did. The body goes
		// to the notification row, where "downloaded" alone is the kind
		// of message a user files and forgets.
		Reason:   describeRelease(*state.Selected),
		Attempts: state.Attempt,
		Selected: state.Selected,
	}

	return finishWith(ctx, in, state, result, storage.NotificationCompleted)
}

// refreshPlex asks the server to rescan the directory the download landed in.
//
// The path comes from the torrent client, and passing it narrows the rescan to
// one directory. Without it Plex rescans the whole library, which on a large
// one takes minutes of disk thrashing after every completed download.
func refreshPlex(ctx workflow.Context, in *Input, contentPath string) {
	if contentPath == "" {
		workflow.GetLogger(ctx).Warn(
			"the torrent client reported no download path; skipping the plex refresh",
			"request", in.RequestID)
		return
	}

	actCtx := withOptions(ctx, plexOptions())

	var ignored struct{}
	if err := workflow.ExecuteActivity(actCtx, (*Activities).RefreshPlex, RefreshPlexInput{
		Spec: in.Spec,
		Path: contentPath,
	}).Get(actCtx, &ignored); err != nil {
		workflow.GetLogger(ctx).Warn("plex refresh failed",
			"request", in.RequestID, "path", contentPath, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Terminal transitions
// ---------------------------------------------------------------------------

// failedResult ends the request unsuccessfully.
func failedResult(ctx workflow.Context, in *Input, state *State, reason string) (*Result, error) {
	result := Result{
		RequestID: in.RequestID,
		Outcome:   storage.StateFailed,
		Title:     in.Spec.DisplayTitle(),
		Year:      in.Spec.Year,
		Reason:    reason,
		Attempts:  state.Attempt,
		Selected:  state.Selected,
	}
	return finishWith(ctx, in, state, result, storage.NotificationFailed)
}

// skippedResult ends the request because the library already has the media.
func skippedResult(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	plex, err := latestPlexMatch(ctx, in)
	if err != nil {
		return nil, err
	}

	result := Result{
		RequestID: in.RequestID,
		Outcome:   storage.StateSkipped,
		Title:     in.Spec.DisplayTitle(),
		Year:      in.Spec.Year,
		Attempts:  state.Attempt,
		PlexTitle: plex.Title,
		PlexYear:  plex.Year,
		Reason:    "already in the Plex library",
	}
	return finishWith(ctx, in, state, result, storage.NotificationSkipped)
}

// finishSkipped records that the library already had the media.
func finishSkipped(
	ctx workflow.Context,
	in Input,
	state *State,
	plex CheckPlexResult,
) (Result, error) {
	result := Result{
		RequestID: in.RequestID,
		Outcome:   storage.StateSkipped,
		Title:     in.Spec.DisplayTitle(),
		Year:      in.Spec.Year,
		Attempts:  state.Attempt,
		PlexTitle: plex.Title,
		PlexYear:  plex.Year,
		Reason:    "already in the Plex library",
	}

	res, err := finishWith(ctx, &in, state, result, storage.NotificationSkipped)
	if err != nil {
		return Result{}, err
	}
	return *res, nil
}

// finishWith writes the terminal state, notifies the watchers and returns the
// result.
//
// Notification comes after the state write, and both happen before the
// workflow returns. A workflow that returned first and notified afterwards
// would lose the notification whenever the process died in between — and "the
// download finished but nobody was told" is the failure this project exists to
// prevent.
func finishWith(
	ctx workflow.Context,
	in *Input,
	state *State,
	result Result,
	kind storage.NotificationKind,
) (*Result, error) {
	state.Phase = result.Outcome

	if err := saveState(ctx, in, state, "request."+string(result.Outcome), result.Reason); err != nil {
		return nil, err
	}

	body := result.Reason
	if result.Outcome == storage.StateSkipped && result.PlexTitle != "" {
		// Naming what matched is what makes a wrong match visible instead of
		// silently swallowing the request.
		body = fmt.Sprintf("Already in Plex as %q (%d)", result.PlexTitle, result.PlexYear)
	}

	if err := notify(ctx, in, kind, titleFor(result), body); err != nil {
		// A notification failure must not lose the outcome. The state is
		// written and the result is returned either way.
		workflow.GetLogger(ctx).Error("could not notify watchers",
			"request", in.RequestID, "error", err)
	}

	return &result, nil
}

// cancelRequest compensates for a user cancellation and returns the outcome.
//
// A cancelled request returns a normal result rather than an error. The caller
// gets something it can show, and every terminal outcome here — completed,
// skipped, failed, cancelled — is a successful evaluation that reached a
// conclusion.
func cancelRequest(ctx workflow.Context, in *Input, state *State) (*Result, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("request cancelled", "request", in.RequestID)

	reason := "cancelled by the user"

	// Compensation runs in a disconnected context. Without that, the
	// cancellation that triggered it would also cancel the cleanup, and the
	// torrent would be left in the client — which is exactly what a user
	// cancelling a download is trying to avoid.
	if state.QbitHash != "" {
		dctx, _ := workflow.NewDisconnectedContext(ctx)
		dctx = withOptions(dctx, deleteOptions())

		var ignored struct{}
		if err := workflow.ExecuteActivity(dctx, (*Activities).DeleteTorrent, DeleteTorrentInput{
			RequestID: in.RequestID,
			Hash:      state.QbitHash,
			// The files go too: a cancelled download has not been filed into
			// the library, so there is nothing here the user is watching.
			DeleteFiles: true,
		}).Get(dctx, &ignored); err != nil {
			// The request is still cancelled. Failing to clean up the client
			// must not resurrect it.
			logger.Error("could not remove the torrent while cancelling",
				"request", in.RequestID, "hash", state.QbitHash, "error", err)
			reason = "cancelled by the user; the torrent could not be removed from the client"
		}
	}

	result := Result{
		RequestID: in.RequestID,
		Outcome:   storage.StateCanceled,
		Title:     in.Spec.DisplayTitle(),
		Year:      in.Spec.Year,
		Reason:    reason,
		Attempts:  state.Attempt,
		Selected:  state.Selected,
	}

	// The state write and the notification also need the disconnected context:
	// the workflow's own context is already done, and every call made with it
	// would fail immediately.
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	return finishWith(dctx, in, state, result, storage.NotificationCanceled)
}

// handleAddFailure decides what an add failure means.
//
// The release is not blacklisted: a client that refuses a torrent is an
// infrastructure problem, not evidence that the release is bad.
func handleAddFailure(ctx workflow.Context, in *Input, state *State, err error) error {
	state.ErrorStreak++
	state.Selected = nil
	state.QbitHash = ""
	state.Phase = storage.StatePending
	state.NextRunAt = workflow.Now(ctx).Add(indexerBackoff(state.ErrorStreak))

	return saveState(ctx, in, state, "torrent.add_failed",
		describeError(err, "could not add the torrent"))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func initialState(ctx workflow.Context, in Input) State {
	phase := in.StartPhase
	if phase == "" {
		phase = storage.StatePending
	}

	now := workflow.Now(ctx)

	nextRunAt := in.StartNextRunAt
	if nextRunAt.IsZero() {
		nextRunAt = now
	}

	deadline := in.StartDownloadDeadline
	if deadline.IsZero() && in.StartQbitHash != "" {
		// A download that was in flight when the history was reset keeps its
		// original two days rather than being handed a fresh budget.
		deadline = now.Add(in.Policy.DownloadDeadline)
	}

	// The search window, for the same reason and with a worse failure if it is
	// dropped. Recomputing it here would mean every ContinueAsNew bought the
	// request another two years, so the deadline would never arrive and the
	// only thing left bounding the execution would be
	// WorkflowExecutionTimeout — which terminates rather than returns.
	searchDeadline := in.StartSearchDeadline
	if searchDeadline.IsZero() {
		searchDeadline = now.Add(in.Policy.SearchDeadline)
	}

	return State{
		Phase:            phase,
		Attempt:          in.StartAttempt,
		ErrorStreak:      in.StartErrorStreak,
		StallCount:       in.StartStallCount,
		Iteration:        in.StartIteration,
		NextRunAt:        nextRunAt,
		Selected:         in.StartSelected,
		QbitHash:         in.StartQbitHash,
		DownloadDeadline: deadline,
		SearchDeadline:   searchDeadline,
		// Carried for the same reason as the deadlines above. The first
		// start of the workflow has ManualSince zero; the workflow enters
		// StateManual by writing the field, and a ContinueAsNew carries
		// it across.
		ManualSince:      in.StartManualSince,
	}
}

// cancellationIsDue reports whether an activity error is really the user
// cancelling rather than a failure.
//
// It matters more than it looks. Activities in flight when a cancellation
// lands are themselves cancelled, and their error arrives shaped like any
// other failure. Treating it as one sends the workflow down the retry path,
// where the next thing it does is write state — with a context that is already
// done, so that write fails too, and the failure propagates out of the
// workflow. The user who cancelled then gets an error result instead of a
// clean cancellation.
func cancellationIsDue(ctx workflow.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, workflow.ErrCanceled) ||
		temporal.IsCanceledError(err)
}

// waitUntil sleeps until an instant, and reports whether it was cancelled
// instead.
//
// workflow.Sleep would be shorter but cannot be interrupted, so a request
// sitting in a daily backoff would take a day to notice a cancellation.
func waitUntil(ctx workflow.Context, t time.Time) (cancelled bool, err error) {
	if ctx.Err() != nil {
		return true, nil
	}

	delay := t.Sub(workflow.Now(ctx))
	if delay <= 0 {
		return false, nil
	}

	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()

	timer := workflow.NewTimer(timerCtx, delay)

	var cancelledByChannel bool
	workflow.NewSelector(ctx).
		AddFuture(timer, func(f workflow.Future) {
			// An error here means the timer was cancelled, which is the other
			// side of the race and carries no further information.
			_ = f.Get(ctx, nil)
		}).
		AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {
			cancelledByChannel = true
		}).
		Select(ctx)

	// ctx.Err() is checked as well as the flag, because when a cancellation
	// lands the timer future and the done channel can both be ready and
	// Select picks among ready cases arbitrarily.
	if cancelledByChannel || ctx.Err() != nil {
		return true, nil
	}
	return false, nil
}

// continueAsNewIfNeeded restarts the history when it has grown too large.
//
// The server warns at 10 240 events and terminates at 51 200, and termination
// loses the request rather than reporting an error. An hourly loop costs
// roughly a dozen events, so without this a two-year request would blow
// through the limit several times over.
func continueAsNewIfNeeded(ctx workflow.Context, in Input, state State) (bool, error) {
	info := workflow.GetInfo(ctx)

	tooLong := info.GetContinueAsNewSuggested() ||
		info.GetCurrentHistoryLength() > in.Policy.ContinueAsNewAt
	if !tooLong {
		return false, nil
	}

	workflow.GetLogger(ctx).Info("restarting the workflow to bound its history",
		"request", in.RequestID,
		"iteration", state.Iteration,
		"history_events", info.GetCurrentHistoryLength())

	// Anything in flight would be dropped by the restart, and a cancellation
	// arriving at this exact moment is one the user would reasonably expect to
	// work.
	if err := workflow.Await(ctx, func() bool {
		return workflow.AllHandlersFinished(ctx)
	}); err != nil {
		return false, err
	}

	// Recorded before the restart, because after it this history is gone. The
	// event is what makes a restart visible in the audit trail — and, in the
	// tests, what makes it observable at all.
	if err := saveState(ctx, &in, &state, "workflow.continued_as_new",
		fmt.Sprintf("history reached %d events after %d iterations",
			info.GetCurrentHistoryLength(), state.Iteration)); err != nil {
		return false, err
	}

	next := in
	next.StartPhase = state.Phase
	next.StartAttempt = state.Attempt
	next.StartErrorStreak = state.ErrorStreak
	next.StartStallCount = state.StallCount
	next.StartNextRunAt = state.NextRunAt
	next.StartSelected = state.Selected
	next.StartQbitHash = state.QbitHash
	next.StartDownloadDeadline = state.DownloadDeadline
	// Carried, not recomputed. Without this the restarted workflow would give
	// itself a fresh two-year window and the request would never reach its
	// deadline at all.
	next.StartSearchDeadline = state.SearchDeadline
	// Same reasoning for the manual-mode timeout: a ContinueAsNew during
	// StateManual would otherwise reset the 14-day wait on every restart.
	next.StartManualSince = state.ManualSince
	// The iteration count has to survive too, or the log line above would
	// report a number that resets at every restart.
	next.StartIteration = state.Iteration

	return true, workflow.NewContinueAsNewError(ctx, Workflow, next)
}

// backoffFor returns how long to wait before the next search.
//
// The ladder is indexed by the number of fruitless searches, so it grows to a
// daily cadence and stays there. There is no attempt ceiling: over two years
// any fixed count would be reached and would become the real deadline.
func backoffFor(p Policy, attempt int) time.Duration {
	if len(p.BackoffLadder) == 0 {
		return time.Hour
	}
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(p.BackoffLadder) {
		idx = len(p.BackoffLadder) - 1
	}
	return p.BackoffLadder[idx]
}

// backoffLadderTop is the longest wait in the ladder.
func backoffLadderTop(p Policy) time.Duration {
	if len(p.BackoffLadder) == 0 {
		return time.Hour
	}
	return p.BackoffLadder[len(p.BackoffLadder)-1]
}

// indexerBackoff backs off a repeated infrastructure failure.
//
// It is exponential from five minutes to six hours, and deliberately separate
// from the search ladder: an indexer being down for ten minutes is far more
// likely than a film appearing and disappearing, and treating the two the same
// way either hammers a broken tracker or gives up on a healthy one.
func indexerBackoff(streak int) time.Duration {
	const (
		base = 5 * time.Minute
		max  = 6 * time.Hour
	)
	if streak <= 0 {
		return base
	}

	d := base
	for i := 1; i < streak && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// notYetReleased reports whether a film is still in the future, and when to
// look again.
//
// This is the dominant case in practice: someone asks for a film that is still
// in cinemas. Without it the loop burns a search an hour for months and
// eventually fails a request that was always going to succeed on its own.
func notYetReleased(spec domain.MediaSpec, now time.Time) (time.Time, bool) {
	if spec.ReleasedAt == nil {
		return time.Time{}, false
	}

	released := *spec.ReleasedAt
	if !released.After(now) {
		return time.Time{}, false
	}

	// Two days after release is when a decent copy starts to circulate. The
	// seven-day cap keeps a wrong release date from pushing the request out by
	// years.
	next := released.Add(48 * time.Hour)
	if cap := now.Add(7 * 24 * time.Hour); next.After(cap) {
		next = cap
	}
	return next, true
}

// checkPlex asks whether the library has the media.
func checkPlex(ctx workflow.Context, in Input, force bool) (CheckPlexResult, error) {
	actCtx := withOptions(ctx, plexOptions())

	var res CheckPlexResult
	err := workflow.ExecuteActivity(actCtx, (*Activities).CheckPlex, CheckPlexInput{
		RequestID: in.RequestID,
		Spec:      in.Spec,
		Force:     force,
		MaxAge:    6 * time.Hour,
	}).Get(actCtx, &res)
	if err != nil {
		return CheckPlexResult{}, err
	}
	return res, nil
}

// latestPlexMatch reads back what the last Plex check found, so a skip can
// name it.
func latestPlexMatch(ctx workflow.Context, in *Input) (CheckPlexResult, error) {
	actCtx := withOptions(ctx, plexOptions())

	var res CheckPlexResult
	err := workflow.ExecuteActivity(actCtx, (*Activities).CheckPlex, CheckPlexInput{
		RequestID: in.RequestID,
		Spec:      in.Spec,
		// Not forced: the answer was written moments ago by the check that
		// decided to skip.
		MaxAge: time.Hour,
	}).Get(actCtx, &res)
	return res, err
}

// saveState writes the projection and records an event.
func saveState(ctx workflow.Context, in *Input, state *State, event, detail string) error {
	actCtx := withOptions(ctx, dbOptions())

	selected := storage.RequestState{
		ID:          in.RequestID,
		State:       state.Phase,
		Attempt:     state.Attempt,
		ErrorStreak: state.ErrorStreak,
		StallCount:  state.StallCount,
		QbitHash:    state.QbitHash,
	}
	if !state.NextRunAt.IsZero() {
		next := state.NextRunAt
		selected.NextRunAt = &next
	}
	if !state.DownloadDeadline.IsZero() {
		deadline := state.DownloadDeadline
		selected.DownloadDeadline = &deadline
	}
	if state.Selected != nil {
		selected.SelectedTitle = state.Selected.Title
		selected.SelectedIndexer = state.Selected.Indexer
		selected.SelectedGUID = state.Selected.GUID
		selected.SelectedInfoHash = state.Selected.InfoHash
		selected.SelectedSize = state.Selected.SizeBytes
		selected.SelectedSeeders = state.Selected.Seeders
	}

	payload := map[string]string{}
	if detail != "" {
		payload["detail"] = detail
		selected.LastError = detail
	}

	return workflow.ExecuteActivity(actCtx, (*Activities).SaveState, SaveStateInput{
		RequestID: in.RequestID,
		State:     selected,
		Event:     event,
		Actor:     "workflow",
		Detail:    payload,
	}).Get(actCtx, nil)
}

// notify tells the watchers what happened.
func notify(
	ctx workflow.Context,
	in *Input,
	kind storage.NotificationKind,
	title, body string,
) error {
	actCtx := withOptions(ctx, notifyOptions())

	return workflow.ExecuteActivity(actCtx, (*Activities).NotifyWatchers, NotifyInput{
		RequestID: in.RequestID,
		Kind:      kind,
		Title:     title,
		Body:      body,
	}).Get(actCtx, nil)
}

func titleFor(result Result) string {
	switch result.Outcome {
	case storage.StateCompleted:
		return "Ready: " + result.Title
	case storage.StateSkipped:
		return "Already in your library: " + result.Title
	case storage.StateFailed:
		return "Gave up on " + result.Title
	case storage.StateCanceled:
		return "Cancelled: " + result.Title
	default:
		return result.Title
	}
}

func describeRelease(r domain.Release) string {
	size := float64(r.SizeBytes) / (1 << 30)
	return fmt.Sprintf("%s — %.2f GB, %d seeders, from %s", r.Title, size, r.Seeders, r.Indexer)
}

// describeError pulls a readable message out of an activity failure.
//
// Temporal wraps activity errors in an ApplicationError whose message is the
// original error text; the raw error's own Error() is a longer, less useful
// string that names the activity type and the attempt count.
func describeError(err error, fallback string) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return appErr.Message()
	}
	if err != nil {
		return err.Error()
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Task queue selection
//
// Each of these returns activity options pinned to a queue. The pinning is not
// optional: an activity scheduled without an explicit queue runs on the
// workflow's own queue, which would silently bypass the rate limit that the
// queue exists to enforce.
// ---------------------------------------------------------------------------

func withOptions(ctx workflow.Context, opts workflow.ActivityOptions) workflow.Context {
	return workflow.WithActivityOptions(ctx, opts)
}

func indexerOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueueIndexer,
		StartToCloseTimeout: 2 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    2 * time.Minute,
			// The queue's own rate limit is the real throttle, so a handful of
			// attempts is plenty; beyond that the circuit breaker inside the
			// activity has already decided the tracker is down.
			MaximumAttempts: 3,
		},
	}
}

func qbitOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueueQBittorrent,
		StartToCloseTimeout: 3 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    3 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    5,
		},
	}
}

func deleteOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueueQBittorrent,
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    15 * time.Second,
			MaximumAttempts:    4,
		},
	}
}

func plexOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueuePlex,
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

func dbOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueueDB,
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    10,
		},
	}
}

func notifyOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           temporalx.TaskQueueNotify,
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    5 * time.Minute,
			// Notification is the one thing that must not be lost: the whole
			// point of the project is being told when the thing you asked for
			// is ready. Unlimited attempts is safe because a duplicate
			// notification is a far smaller problem than a missing one.
			MaximumAttempts: 0,
		},
	}
}

// isNoDiskSpaceError reports whether the error from AddTorrent was the
// "out of disk space" sentinel raised by the qBittorrent client.
//
// The check is on the application-error type rather than the wrapped
// error: the activity wraps qbit.ErrNoSpace into a temporal.ApplicationError
// with type "NoDiskSpace", and a workflow that branches on the cause has
// to look at that type rather than at the message — strings change, type
// names do not.
func isNoDiskSpaceError(err error) bool {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return false
	}
	return appErr.Type() == "NoDiskSpace"
}

// manualReleaseFromLink builds a synthetic Release around a user-submitted
// link so the existing AddTorrent path can take over without the scoring
// step. A magnet goes into MagnetURI, anything else into DownloadURL;
// AddTorrent calls Release.Link() to pick the right field.
func manualReleaseFromLink(link, title string) *domain.Release {
	if strings.HasPrefix(link, "magnet:") {
		return &domain.Release{
			Title:     title,
			Indexer:   "manual",
			MagnetURI: link,
		}
	}
	return &domain.Release{
		Title:       title,
		Indexer:     "manual",
		DownloadURL: link,
	}
}
