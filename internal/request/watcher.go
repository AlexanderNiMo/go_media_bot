package request

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/temporalx"
)

// The download watcher exists to solve a problem that has nothing to do with
// torrents: a two-day download polled at the cadence the interface needs would
// cost about fifty thousand history events, and the server terminates a
// workflow at fifty-one thousand.
//
// It splits one job between two components with different needs. The interface
// wants progress every thirty seconds. The request workflow only wants to know
// done, stalled or expired, and five minutes is plenty for that. So this polls
// the client at the fast cadence and writes to the projection, and the request
// workflow reads its own row at the slow one.
//
// The second benefit is that exactly one component talks to qBittorrent.
// Its authentication is a session cookie with server-side invalidation and an
// IP ban after repeated failed logins, so a second caller polling the same
// client is not just wasteful, it is a way to lock everyone out.

// DownloadWatcherWorkflow runs the watcher.
//
// It is a singleton: started once with a fixed workflow ID, and kept running
// for the lifetime of the deployment. Restarting the process does not restart
// it — Temporal resumes it — and a second attempt to start it attaches to the
// running one instead of creating a rival.
//
// Each iteration runs one long activity. The activity polls internally and
// returns after an hour, or sooner if the worker is shutting down. That keeps
// the history cost at roughly twenty-four activity starts a day rather than
// one per poll.
func DownloadWatcherWorkflow(ctx workflow.Context, in WatcherInput) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("download watcher started",
		"interval", in.Interval, "max_run", in.MaxRun)

	guard := in.GuardDelay
	if guard <= 0 {
		guard = 5 * time.Second
	}

	for {
		// The restart is checked here rather than inside the activity, because
		// the activity is where the wall-clock time is spent and the workflow
		// is where the history accumulates.
		if done, err := watcherContinueAsNew(ctx, in); done || err != nil {
			return err
		}

		actCtx := workflow.WithActivityOptions(ctx, watcherOptions(in))

		err := workflow.ExecuteActivity(actCtx, (*Activities).WatchDownloads, WatchInput{
			Interval: in.Interval,
			MaxRun:   in.MaxRun,
		}).Get(actCtx, nil)

		if err != nil {
			if temporal.IsCanceledError(err) {
				return nil
			}
			// The activity already reports its own per-cycle failures and
			// keeps going; an error reaching here means the whole activity
			// died, which is usually the database being unreachable. Logging
			// and looping is right: the retry policy inside the activity
			// handles transient trouble, and this handles the rest.
			logger.Error("the download watcher activity failed; retrying",
				"error", err)
		}

		// A guard against a hot loop.
		//
		// The activity is built to run for MaxRun, so normally this never
		// fires for long. But it returns early when the worker is shutting
		// down, and it returns instantly if it is misconfigured — and in both
		// cases the loop above would call it again immediately, spinning
		// without ever yielding and accumulating history events as fast as the
		// server accepts them. A few seconds between invocations costs one
		// timer event an hour in the normal case and bounds the damage in the
		// abnormal one.
		if err := workflow.Sleep(ctx, guard); err != nil {
			// Only a cancellation returns here, and there is nothing to
			// report: the watcher has no request outcome.
			return nil
		}
	}
}

// WatcherInput configures the watcher.
type WatcherInput struct {
	// Interval is how often the torrent client is polled. This is the cadence
	// the interface needs, not the one the workflow needs.
	Interval time.Duration
	// MaxRun bounds one activity invocation. It is what keeps the history
	// small, and it must stay well under the activity's start-to-close timeout.
	MaxRun time.Duration
	// ContinueAsNewAt is the history length at which the workflow restarts
	// itself, the same as for a request.
	ContinueAsNewAt int
	// GuardDelay is the pause between invocations, purely to stop a hot loop
	// if the activity returns sooner than expected. Zero means five seconds.
	GuardDelay time.Duration
}

// WatchInput is one invocation of the watcher activity.
type WatchInput struct {
	Interval time.Duration
	MaxRun   time.Duration
}

// watcherOptions builds the activity options for the watcher.
func watcherOptions(in WatcherInput) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// Its own queue, with a concurrency of one: there is exactly one
		// watcher, and it must not compete with the activities that add and
		// remove torrents for the same client session.
		TaskQueue:           temporalx.TaskQueueQbitWatch,
		StartToCloseTimeout: in.MaxRun + 5*time.Minute,
		// The heartbeat is what carries cancellation into the activity, and
		// what lets the server tell a slow poll from a dead worker. It has to
		// be comfortably longer than the interval between the activity's own
		// heartbeats, or a healthy activity would be judged dead.
		HeartbeatTimeout: in.Interval * 3,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    30 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    5 * time.Minute,
			// Unlimited. The watcher is the only thing that notices a finished
			// download, and giving up on it would leave requests waiting on a
			// torrent that finished hours ago.
			MaximumAttempts: 0,
		},
	}
}

// watcherContinueAsNew restarts the workflow's history when it grows.
func watcherContinueAsNew(ctx workflow.Context, in WatcherInput) (bool, error) {
	info := workflow.GetInfo(ctx)

	threshold := in.ContinueAsNewAt
	if threshold <= 0 {
		threshold = 5000
	}

	if !info.GetContinueAsNewSuggested() && info.GetCurrentHistoryLength() <= threshold {
		return false, nil
	}

	workflow.GetLogger(ctx).Info("restarting the download watcher to bound its history",
		"history_events", info.GetCurrentHistoryLength())

	return true, workflow.NewContinueAsNewError(ctx, DownloadWatcherWorkflow, in)
}

// ---------------------------------------------------------------------------
// The activity
// ---------------------------------------------------------------------------

// WatchDownloads polls the torrent client and records what it finds.
//
// It runs for up to MaxRun, polling every Interval, and returns when that
// elapses — not when there is nothing to watch. Idling for an hour and
// returning is deliberate: it costs one history event per hour, and it means a
// download added a second after the last poll is picked up on the next one
// rather than when something notices it should start watching again.
func (a *Activities) WatchDownloads(ctx context.Context, in WatchInput) error {
	interval := in.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	maxRun := in.MaxRun
	if maxRun <= 0 {
		maxRun = time.Hour
	}

	log := a.log.With("component", "download-watcher")
	deadline := time.Now().Add(maxRun)

	// workerStop closes when the process is shutting down, which gives the
	// activity a chance to return cleanly instead of being killed at the
	// start-to-close timeout.
	workerStop := activity.GetWorkerStopChannel(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// One poll immediately, so a freshly started watcher does not sit idle for
	// a whole interval while a download is already running.
	if err := a.pollDownloads(ctx); err != nil {
		log.Warn("the first download poll failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			// The workflow was cancelled, or the activity was asked to stop.
			return ctx.Err()

		case <-workerStop:
			log.Info("the worker is stopping; ending the download watch early")
			return nil

		case <-ticker.C:
			// Recorded before the work, so a slow poll is not mistaken for a
			// dead worker.
			activity.RecordHeartbeat(ctx, time.Until(deadline).Round(time.Second).String())

			if err := a.pollDownloads(ctx); err != nil {
				// Logged and swallowed: one failed poll is not a reason to
				// tear down the activity. If the database is genuinely down,
				// every subsequent poll fails too and the error surfaces
				// through the workflow's log at a rate that is readable.
				log.Warn("download poll failed", "error", err)
			}

			if time.Now().After(deadline) {
				return nil
			}
		}
	}
}

// pollDownloads performs one polling cycle.
func (a *Activities) pollDownloads(ctx context.Context) error {
	downloads, err := a.store.ListActiveDownloads(ctx)
	if err != nil {
		return err
	}
	if len(downloads) == 0 {
		// Nothing to watch. The list is re-read on the next cycle, so a
		// download added in the meantime is picked up then.
		return nil
	}

	hashes := make([]string, 0, len(downloads))
	for _, d := range downloads {
		hashes = append(hashes, d.Hash)
	}

	// One request for every download rather than one per download. The API is
	// built for this, and it is what keeps the traffic to the client flat as
	// the number of active downloads grows.
	statuses, err := a.qbit.Info(ctx, hashes)
	if err != nil {
		return fmt.Errorf("poll the torrent client: %w", err)
	}

	now := time.Now()
	for _, d := range downloads {
		status, present := statuses[d.Hash]
		if !present {
			a.recordMissing(ctx, d, now)
			continue
		}
		a.recordPresent(ctx, d, status, now)
	}
	return nil
}

// recordPresent writes what the client says about a torrent that is there.
func (a *Activities) recordPresent(ctx context.Context, d storage.ActiveDownload, s domain.TorrentStatus, now time.Time) {
	facts := storage.DownloadFacts{
		Progress:    s.Progress,
		DlSpeed:     s.DlSpeed,
		ETASeconds:  s.ETA,
		QbitState:   s.State,
		ContentPath: s.ContentPath,
		// It is back, so it is not missing.
		MissingSince: nil,
	}

	// zero_since tracks whether the download is moving, not whether it is at
	// zero. A torrent stopped at 98% is as stuck as one stopped at nothing,
	// and the request workflow's stall check is what turns either into an
	// abandoned release.
	switch {
	case s.Done():
		// A finished torrent is not stalled, whatever the client calls the
		// state, and it has no meaningful speed or estimate left.
		facts.Progress = 1.0
		facts.DlSpeed = 0
		facts.ETASeconds = 0
		facts.ZeroSince = nil

	case s.Failed():
		// The client has given up. Recording a stall as well is deliberate:
		// the request workflow's stall path is what abandons the release and
		// searches again, and it needs a timestamp to act on.
		facts.ZeroSince = carriedSince(d.ZeroSince, now)

	case s.Stalled(), s.Progress <= d.Progress:
		// Not moving: either the client says so, or the progress has not
		// advanced since the last poll. The second condition is what catches a
		// torrent that is nominally downloading but has stalled with no peers.
		facts.ZeroSince = carriedSince(d.ZeroSince, now)

	default:
		// Moving.
		facts.ZeroSince = nil
	}

	if err := a.store.UpdateDownloadFacts(ctx, d.RequestID, facts); err != nil {
		a.log.Warn("could not record download progress",
			"request", d.RequestID, "error", err)
	}
}

// recordMissing notes that a torrent the request expects is not in the client.
//
// A single miss proves nothing — an add is asynchronous, and a client that is
// restarting reports nothing for a moment. So the first miss only records when
// it started, and the request workflow applies a grace period before acting.
func (a *Activities) recordMissing(ctx context.Context, d storage.ActiveDownload, now time.Time) {
	facts := storage.DownloadFacts{
		// The last known values are kept rather than zeroed: how far the
		// download got before it vanished is what decides whether the release
		// is worth blacklisting, and zeroing it would erase that.
		Progress:     d.Progress,
		QbitState:    "missing",
		ZeroSince:    d.ZeroSince,
		MissingSince: carriedSince(d.MissingSince, now),
	}

	if err := a.store.UpdateDownloadFacts(ctx, d.RequestID, facts); err != nil {
		a.log.Warn("could not record a missing torrent",
			"request", d.RequestID, "error", err)
	}
}

// carriedSince returns the existing timestamp, or the current time if there is
// none.
//
// It is what makes the timestamps mean "since when" rather than "as of now":
// rewriting them on every poll would mean a stalled download's clock restarted
// every thirty seconds and the stall would never be old enough to act on.
func carriedSince(existing *time.Time, now time.Time) *time.Time {
	if existing != nil {
		return existing
	}
	return &now
}
