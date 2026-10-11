package request

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/temporalx"
)

// Module wires the workflow and its activities into the fx graph.
var Module = fx.Module("request",
	fx.Provide(NewActivities),
	fx.Provide(ProfileFromConfig),
	fx.Provide(NewService),
)

// WorkerModule registers the workflow and activities on the temporal worker.
//
// It is separate from Module because the connectivity-free commands — migrate,
// and the read-only CLI commands — must be able to build the graph without a
// worker.
var WorkerModule = fx.Module("request-worker",
	fx.Invoke(registerWorker),
)

// queueSpec is one task queue and the limits it runs under.
type queueSpec struct {
	name string
	// activitiesPerSecond is enforced by the server across every worker
	// polling the queue, which is what distinguishes it from the per-process
	// limit.
	activitiesPerSecond float64
	// maxConcurrentActivities is the other, independent control: the rate
	// limit caps how often work starts, this caps how much runs at once.
	maxConcurrentActivities int
}

// registerWorker builds one worker per task queue and ties them to the
// application lifecycle.
//
// One worker per queue rather than one queue for everything, because each
// downstream service needs a different limit. The limits are the reason this
// function is longer than a registration call would be.
func registerWorker(lc fx.Lifecycle, c client.Client, cfg *config.Config, acts *Activities, log *slog.Logger) error {
	specs := []queueSpec{
		{
			// The workflow queue. Workflows themselves cost nothing while
			// parked in a timer, which is where these spend almost all of a
			// two-year life.
			name:                    cfg.Temporal.TaskQueue,
			maxConcurrentActivities: 0, // activities never run here
		},
		{
			// The rate-limited indexer queue.
			//
			// The limit is set below the real target on purpose. Prowlarr fans
			// every request out across all configured trackers, and the server
			// spreads activity tasks across several partitions of a queue and
			// can burst above the configured average — so a limit set exactly
			// at the tracker's tolerance will occasionally exceed it, and the
			// consequence of that is a banned IP rather than a slow request.
			name:                    temporalx.TaskQueueIndexer,
			activitiesPerSecond:     cfg.Indexer.RatePerSecond,
			maxConcurrentActivities: 1,
		},
		{
			// The torrent client. Concurrency of one because qBittorrent
			// authentication is a session cookie with server-side invalidation
			// and an IP ban after repeated failed logins: one caller means one
			// session and one serialised re-login.
			name:                    temporalx.TaskQueueQBittorrent,
			maxConcurrentActivities: 1,
		},
		{
			name:                    temporalx.TaskQueueQbitWatch,
			maxConcurrentActivities: 1,
		},
		{
			name:                    temporalx.TaskQueuePlex,
			maxConcurrentActivities: 4,
		},
		{
			name:                    temporalx.TaskQueueNotify,
			maxConcurrentActivities: 2,
		},
		{
			// Short database work. The concurrency is generous because each
			// call is a single indexed statement.
			name:                    temporalx.TaskQueueDB,
			maxConcurrentActivities: 8,
		},
	}

	var workers []worker.Worker

	for _, spec := range specs {
		opts := worker.Options{
			// The SDK default is zero, which it passes straight into a timer
			// that fires immediately — so Stop returns before in-flight
			// activities finish and the goroutines keep holding their slots.
			WorkerStopTimeout: 30 * time.Second,
		}
		if spec.maxConcurrentActivities > 0 {
			opts.MaxConcurrentActivityExecutionSize = spec.maxConcurrentActivities
		}
		if spec.activitiesPerSecond > 0 {
			opts.TaskQueueActivitiesPerSecond = spec.activitiesPerSecond
		}

		w := worker.New(c, spec.name, opts)

		if spec.name == cfg.Temporal.TaskQueue {
			w.RegisterWorkflowWithOptions(Workflow, workflow.RegisterOptions{Name: WorkflowType})
		}

		// Registering the struct registers every exported method as an
		// activity, so each queue gets the whole set. Temporal routes a call
		// by the queue named in the activity options, not by which worker
		// registered it, so over-registering costs nothing and avoids a class
		// of bug where an activity is scheduled onto a queue that never
		// registered it — which produces no error anywhere, the activity is
		// simply never picked up.
		w.RegisterActivity(acts)

		workers = append(workers, w)
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			// Start does not block; it begins polling in the background.
			for i, w := range workers {
				if err := w.Start(); err != nil {
					// Stop whatever already started, so a partial failure does
					// not leave the process half polling.
					for _, started := range workers[:i] {
						started.Stop()
					}
					return fmt.Errorf("start the worker for task queue %q: %w", specs[i].name, err)
				}
			}
			log.Info("worker started", "queues", len(workers), "namespace", cfg.Temporal.Namespace)
			return nil
		},
		OnStop: func(context.Context) error {
			// Reverse order, so the queues that feed the projection stop
			// before the one that writes workflows to it.
			for i := len(workers) - 1; i >= 0; i-- {
				workers[i].Stop()
			}
			return nil
		},
	})

	return nil
}

// ProfileFromConfig builds the release policy from configuration.
func ProfileFromConfig(cfg *config.Config) release.Profile {
	return release.ProfileFromConfig(cfg)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service starts and controls request workflows.
//
// It is the single entry point the transports — the CLI, the HTTP API and the
// Telegram bot — use, so that the rules about deduplication and workflow
// identity live in one place rather than three.
type Service struct {
	temporal client.Client
	store    *storage.Store
	profile  release.Profile
	cfg      *config.Config
}

// NewService builds the service layer.
func NewService(c client.Client, store *storage.Store, profile release.Profile, cfg *config.Config) *Service {
	return &Service{temporal: c, store: store, profile: profile, cfg: cfg}
}

// StartInput is a request to fetch one piece of media.
type StartInput struct {
	Spec   domain.MediaSpec
	UserID int64
	// Force starts a new workflow even when one is already running, which is
	// what a user needs when the library lost the file.
	Force bool
}

// StartRequest creates the projection row and starts the workflow.
//
// The order is deliberate. The row is written first, and its id is what the
// workflow is started with. The reverse — start the workflow, then record it —
// leaves a running workflow that nothing can find if the process dies in
// between, and a two-year execution nobody knows about is worse than a row
// that has to be cleaned up.
func (s *Service) StartRequest(ctx context.Context, in StartInput) (*storage.Request, error) {
	if err := in.Spec.Ref.ValidForRequest(); err != nil {
		return nil, err
	}

	// A request that is already running is reused rather than duplicated.
	// Attaching the new user to it is one INSERT into the watcher table; the
	// running workflow is never told, because it reads its watchers from the
	// database when it has something to say.
	if existing, err := s.store.FindActiveRequest(ctx, in.Spec.Ref); err == nil {
		if !in.Force {
			if in.UserID != 0 {
				if err := s.store.AddWatcher(ctx, existing.ID, in.UserID); err != nil {
					return nil, err
				}
			}
			return s.store.GetRequest(ctx, existing.ID)
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	now := time.Now()
	workflowID := in.Spec.Ref.WorkflowID()

	policy := s.policy()

	req, err := s.store.CreateRequest(ctx, storage.CreateRequestInput{
		Spec:       in.Spec,
		WorkflowID: workflowID,
		// Jittered, so requests created in one batch do not stay aligned
		// forever and turn every hour into a spike against the indexer.
		NextRunAt:      now.Add(jitter(policy.Jitter)),
		SearchDeadline: now.Add(policy.SearchDeadline),
	})
	if err != nil {
		if errors.Is(err, storage.ErrActiveRequestExists) {
			// Lost a race with another caller between the check above and this
			// insert. The database is the arbiter, so the loser attaches to
			// the winner rather than failing.
			existing, findErr := s.store.FindActiveRequest(ctx, in.Spec.Ref)
			if findErr != nil {
				return nil, err
			}
			if in.UserID != 0 {
				if err := s.store.AddWatcher(ctx, existing.ID, in.UserID); err != nil {
					return nil, err
				}
			}
			return s.store.GetRequest(ctx, existing.ID)
		}
		return nil, err
	}

	run, err := s.temporal.ExecuteWorkflow(ctx, s.startOptions(workflowID), Workflow, Input{
		RequestID: req.ID,
		Spec:      in.Spec,
		Policy:    policy,
	})
	if err != nil {
		return nil, fmt.Errorf("start the workflow for %s: %w", in.Spec.Ref, err)
	}

	if err := s.store.SetRunID(ctx, req.ID, run.GetRunID(), run.GetID()); err != nil {
		// The workflow is running and the row exists; only the run id failed
		// to record. That costs a lookup, not a request.
		return nil, err
	}

	if in.UserID != 0 {
		if err := s.store.AddWatcher(ctx, req.ID, in.UserID); err != nil {
			return nil, err
		}
	}

	return s.store.GetRequest(ctx, req.ID)
}

// startOptions builds the workflow start options.
func (s *Service) startOptions(workflowID string) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: s.cfg.Temporal.TaskQueue,

		// A running workflow is reused rather than duplicated, which is the
		// safety net for a race the database did not catch.
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		// A *closed* workflow id may be reused. Without this, re-requesting a
		// film that was deleted from Plex would be impossible for two years.
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,

		// Measured across the whole ContinueAsNew chain, so this is the hard
		// backstop for the product's two-year search deadline. It is set three
		// days beyond it: a release found on the last day still needs its
		// download window, and a timeout is a killed workflow rather than a
		// reported failure.
		WorkflowExecutionTimeout: s.searchDeadline() + 72*time.Hour,
	}
}

// Cancel asks a request's workflow to stop and clean up.
//
// Cancellation is Temporal's own rather than a signal, so the workflow's
// context is what unwinds and the compensation runs in a disconnected context.
// The call returns as soon as the server has recorded the request; the
// workflow unwinds in its own time.
func (s *Service) Cancel(ctx context.Context, requestID int64) error {
	req, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if !req.State.Active() {
		return fmt.Errorf("request %d is already %s", requestID, req.State)
	}

	if err := s.temporal.CancelWorkflow(ctx, req.WorkflowID, ""); err != nil {
		return fmt.Errorf("cancel the workflow for request %d: %w", requestID, err)
	}

	// The projection is updated by the workflow itself as it unwinds, so the
	// row will show "canceled" once the compensation has run. Writing it here
	// as well would race with that.
	return nil
}

// AttachWatcher adds a user to a running request.
func (s *Service) AttachWatcher(ctx context.Context, requestID, userID int64) error {
	return s.store.AddWatcher(ctx, requestID, userID)
}

// DetachWatcher removes a user, cancelling the request when they were the last
// one.
//
// The last-watcher rule is what stops one user's "no thanks" from silently
// destroying two other people's downloads.
func (s *Service) DetachWatcher(ctx context.Context, requestID, userID int64) (canceled bool, err error) {
	last, err := s.store.RemoveWatcher(ctx, requestID, userID)
	if err != nil {
		return false, err
	}
	if !last {
		return false, nil
	}

	if err := s.Cancel(ctx, requestID); err != nil {
		// The watcher is gone either way; a request that could not be
		// cancelled has nobody left to notify, and the reaper for abandoned
		// requests is a separate concern.
		return false, err
	}
	return true, nil
}

// Describe returns the workflow's own view of a request.
//
// It is a query, so it needs a live worker and fails when none is polling —
// which is why nothing on a render path uses it. The interface reads the
// projection; this exists for the CLI's detail view and for debugging.
func (s *Service) Describe(ctx context.Context, requestID int64) (State, error) {
	req, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return State{}, err
	}

	value, err := s.temporal.QueryWorkflow(ctx, req.WorkflowID, "", QueryState)
	if err != nil {
		return State{}, fmt.Errorf("query the workflow for request %d: %w", requestID, err)
	}

	var state State
	if err := value.Get(&state); err != nil {
		return State{}, fmt.Errorf("decode the workflow state: %w", err)
	}
	return state, nil
}

// policy builds the workflow policy from configuration.
func (s *Service) policy() Policy {
	return Policy{
		SearchDeadline:   s.cfg.Search.SearchDeadline,
		BackoffLadder:    s.cfg.Search.BackoffLadder,
		Jitter:           s.cfg.Search.Jitter,
		PollInterval:     s.cfg.Search.PollInterval,
		ContinueAsNewAt:  s.cfg.Search.ContinueAsNewAt,
		DownloadDeadline: s.cfg.Download.Deadline,
		ZeroStallAfter:   s.cfg.Download.ZeroStallAfter,
		MaxStalls:        s.cfg.Download.MaxStalls,
	}
}

func (s *Service) searchDeadline() time.Duration {
	if s.cfg.Search.SearchDeadline > 0 {
		return s.cfg.Search.SearchDeadline
	}
	return 17520 * time.Hour
}

// jitter spreads scheduled runs out.
//
// Without it, requests created in one batch — someone adding a season of ten
// episodes, say — stay aligned forever, and every hour becomes a spike against
// the indexer. The jitter is applied in Go rather than in the workflow because
// a workflow may not call a random source directly.
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}
