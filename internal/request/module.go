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
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
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

// WatcherWorkflowID is the fixed identity of the singleton download watcher.
//
// A fixed id is what makes it a singleton: starting it twice attaches to the
// running one rather than creating a rival, and a restarted process resumes
// the existing execution instead of beginning a second.
const WatcherWorkflowID = "download-watcher"

// startWatcher starts the download watcher, or leaves the running one alone.
//
// It is called from the worker's start hook. The watcher is what notices a
// finished download, so a deployment without one leaves every request waiting
// on a torrent that finished hours ago.
func startWatcher(ctx context.Context, c client.Client, cfg *config.Config, log *slog.Logger) error {
	options := client.StartWorkflowOptions{
		ID:        WatcherWorkflowID,
		TaskQueue: cfg.Temporal.TaskQueue,
		// A running watcher is reused; the policy for a *closed* id is
		// AllowDuplicate so that one that ended — a worker that shut down
		// mid-run, say — does not block a new one from starting.
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	interval := cfg.QBittorrent.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	// The type name is passed as a string rather than as the Go function, and
	// that is not cosmetic. ExecuteWorkflow derives the type from the name of
	// whatever it is handed: given a function it uses the function's own name,
	// consulting only the *client's* registry of aliases — which is empty,
	// because aliases are registered on workers. Passing DownloadWatcherWorkflow
	// here would therefore start a workflow of type "DownloadWatcherWorkflow"
	// that happens to be right only as long as the Go function keeps that name,
	// and passing Workflow in the same position starts one of type "Workflow".
	//
	// A workflow whose type no worker registered is not an error anywhere. It
	// is scheduled, no worker can claim it, and it waits. Silently. For two
	// years.
	run, err := c.ExecuteWorkflow(ctx, options, DownloadWatcherWorkflowType, WatcherInput{
		Interval: interval,
		// An hour per activity invocation. It bounds the history the watcher
		// accumulates and keeps the start-to-close timeout comfortably larger
		// than the work.
		MaxRun:          time.Hour,
		ContinueAsNewAt: cfg.Search.ContinueAsNewAt,
	})
	if err != nil {
		return fmt.Errorf("start the download watcher: %w", err)
	}

	log.Info("download watcher running",
		"workflow_id", run.GetID(), "interval", interval)
	return nil
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
			w.RegisterWorkflowWithOptions(DownloadWatcherWorkflow,
				workflow.RegisterOptions{Name: DownloadWatcherWorkflowType})
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

			// Started separately from the workers, and after them: the watcher
			// immediately schedules an activity, and that activity needs a
			// worker polling its queue or it would wait for the first poll to
			// begin.
			startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := startWatcher(startCtx, c, cfg, log); err != nil {
				return err
			}
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
	temporal workflowClient
	store    *storage.Store
	profile  release.Profile
	cfg      *config.Config
}

// workflowClient is the part of the Temporal client this layer uses.
//
// Narrowed so that what the service *asks the engine to do* can be tested
// without a server — which matters more here than usual, because the two
// mistakes this interface guards against are both invisible at runtime: a
// workflow started under a type name no worker registered, and a start that
// silently attaches to an execution that was already running. Neither produces
// an error anywhere. Both are just a wait that never ends.
type workflowClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
	CancelWorkflow(ctx context.Context, workflowID, runID string) error
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...any) (converter.EncodedValue, error)
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg any) error
}

// startRun starts one execution of the request workflow.
//
// Every start goes through here so that the type name is written once.
func (s *Service) startRun(
	ctx context.Context,
	opts client.StartWorkflowOptions,
	in Input,
) (client.WorkflowRun, error) {
	// By type name, not by function, and this is the call that matters most:
	// it is how every request enters the system. ExecuteWorkflow derives the
	// type from whatever it is handed — given a function it uses the
	// function's own name, which here is "Workflow" — and consults only the
	// client's registry of aliases, which is empty because aliases are
	// registered on workers. So passing Workflow starts a workflow of type
	// "Workflow", which no worker has registered.
	//
	// The consequence of that is not an error. The execution is accepted, no
	// worker can claim its task, and it waits — silently, for as long as its
	// two-year execution timeout.
	return s.temporal.ExecuteWorkflow(ctx, opts, WorkflowType, in)
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

// StartResult is the outcome of asking for something.
type StartResult struct {
	Request *storage.Request
	// Reused reports that an active request for the same media already
	// existed, and this caller was attached to it as a watcher instead of a
	// second download starting.
	//
	// It is part of the result rather than something the caller infers,
	// because the difference is the whole message: "started" and "somebody
	// else is already fetching this, you will be told when it arrives" are not
	// the same thing to be told, and a caller that cannot distinguish them
	// will say the wrong one.
	Reused bool
}

// StartRequest creates the projection row and starts the workflow.
//
// The order is deliberate. The row is written first, and its id is what the
// workflow is started with. The reverse — start the workflow, then record it —
// leaves a running workflow that nothing can find if the process dies in
// between, and a two-year execution nobody knows about is worse than a row
// that has to be cleaned up.
func (s *Service) StartRequest(ctx context.Context, in StartInput) (*StartResult, error) {
	if err := in.Spec.Ref.ValidForRequest(); err != nil {
		return nil, err
	}

	// A request that is already running is reused rather than duplicated.
	// Attaching the new user to it is one INSERT into the watcher table; the
	// running workflow is never told, because it reads its watchers from the
	// database when it has something to say.
	if existing, err := s.store.FindActiveRequest(ctx, in.Spec.Ref); err == nil {
		if !in.Force {
			return s.attachTo(ctx, existing.ID, in.UserID)
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
			return s.attachTo(ctx, existing.ID, in.UserID)
		}
		return nil, err
	}

	run, err := s.startRun(ctx, s.startOptions(workflowID), Input{
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

	stored, err := s.store.GetRequest(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return &StartResult{Request: stored}, nil
}

// attachTo adds a user to a request that is already running.
func (s *Service) attachTo(ctx context.Context, requestID, userID int64) (*StartResult, error) {
	if userID != 0 {
		if err := s.store.AddWatcher(ctx, requestID, userID); err != nil {
			return nil, err
		}
	}

	req, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	return &StartResult{Request: req, Reused: true}, nil
}

// RetryInput is who is restarting what.
//
// The interface and the person are separate because the audit trail separates
// them: the schema pins the actor to a small vocabulary of components, so a
// request restarted by hand reads the same whether it came from the CLI or the
// web, and who did it goes on the event detail where a name belongs.
type RetryInput struct {
	RequestID int64
	// Actor is the interface that asked: "cli", "api" or "telegram".
	Actor string
	// RequestedBy is the person.
	RequestedBy string
}

// Retry restarts a request that has stopped.
//
// It is deliberately a fresh run of the same request rather than a new one:
// the row, its watchers and its audit trail all carry over, so "I asked for
// this in March, it failed, I asked again in June" is one story rather than
// two unrelated ones.
//
// The search window is replaced, not resumed. The two reasons anyone retries
// are that the deadline passed and that too many releases were found wanting,
// and both leave a request with no useful time left — a retry that inherited
// the old deadline would fail on its first pass and look broken. The cost is
// that a request retried by hand can outlive its original two years; that is a
// person deciding, not a loop.
func (s *Service) Retry(ctx context.Context, in RetryInput) (*storage.Request, error) {
	requestID := in.RequestID

	req, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req.State.Active() {
		return nil, fmt.Errorf("request %d is %s and does not need restarting", requestID, req.State)
	}

	// The whole media row, not the copy on the projection. The projection
	// carries what the interface renders; the workflow needs the release date
	// and the external ids as well, and a restarted request that had silently
	// lost them would search for the wrong thing for two years.
	media, err := s.store.GetMedia(ctx, req.MediaID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	// No jitter. Jitter exists to stop requests created in a batch from
	// staying aligned; a retry is one person asking for one thing, and making
	// them wait up to ten minutes to see it start is the wrong trade.
	nextRunAt := now
	deadline := now.Add(s.searchDeadline())

	// The workflow is started before the row is reset, which is the opposite
	// of the order StartRequest uses, and the reason is the failure mode. A
	// row reset without a workflow leaves the request reading "pending"
	// forever — and pending is an active state, so both a second retry and a
	// fresh request would refuse to touch it. The row would be wedged with
	// nothing running to un-wedge it. Started first, the worst case is a
	// workflow whose row still says "failed" for the second it takes it to
	// write its own state.
	run, err := s.startRun(ctx, s.retryOptions(req.WorkflowID), Input{
		RequestID:           req.ID,
		Spec:                media.Spec,
		Policy:              s.policy(),
		StartPhase:          storage.StatePending,
		StartNextRunAt:      nextRunAt,
		StartSearchDeadline: deadline,
	})
	if err != nil {
		// The one refusal worth explaining. It means the projection and the
		// workflow engine disagree — the row says this request has stopped and
		// an execution is still running — which is the state an operator most
		// needs the reason for. Left as the SDK's own message it reads like a
		// bug in the command rather than a stale row.
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
			return nil, fmt.Errorf(
				"request %d already has a workflow running, although its row says %s: "+
					"the projection is behind, so retry once it settles, or cancel it first",
				requestID, req.State)
		}
		return nil, fmt.Errorf("restart the workflow for request %d: %w", requestID, err)
	}

	if _, err := s.store.RestartRequest(ctx, storage.RestartInput{
		ID:               req.ID,
		FromState:        req.State,
		PreviousAttempts: req.AttemptCount,
		PreviousError:    req.LastError,
		NextRunAt:        nextRunAt,
		SearchDeadline:   deadline,
		Actor:            in.Actor,
		RequestedBy:      in.RequestedBy,
	}); err != nil {
		return nil, err
	}

	if err := s.store.SetRunID(ctx, req.ID, run.GetRunID(), run.GetID()); err != nil {
		return nil, err
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

// retryOptions builds the start options for a deliberate restart.
//
// It differs from startOptions in two fields, and both are needed — the first
// without the second does nothing at all.
//
// A retry must not attach to a running execution. With the conflict policy
// StartRequest uses, starting a workflow whose id is already running returns a
// handle to the running one and reports success, so a retry would silently do
// nothing: the row would be reset and the old execution would carry on with
// its old state.
//
// ConflictPolicy FAIL is what tells the *server* to refuse instead. But the
// client then converts that refusal into a handle to the existing run and
// returns it with a nil error, unless WorkflowExecutionErrorWhenAlreadyStarted
// is set. Measured, not assumed: FAIL on its own returns the running run's id
// and no error.
//
// Reuse for *closed* executions stays permissive, or retrying would be
// impossible for the two years the previous run's id is reserved.
func (s *Service) retryOptions(workflowID string) client.StartWorkflowOptions {
	opts := s.startOptions(workflowID)
	opts.WorkflowIDConflictPolicy = enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL
	opts.WorkflowExecutionErrorWhenAlreadyStarted = true
	return opts
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

// SignalPick wakes a request because the user chose a release from the
// candidate list.
//
// The choice itself is already in the database when this is called; the
// signal only tells the workflow that something happened, so that it stops
// waiting out the rest of its backoff ladder and looks.
//
// A request that is no longer running is not an error: the pick stays in
// the database, and a request that comes back — a retry, a restart — will
// find it on its next tick. The signal is an optimisation, never the only
// path to the choice.
func (s *Service) SignalPick(ctx context.Context, requestID int64) error {
	req, err := s.store.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if !req.State.Active() {
		return fmt.Errorf("request %d is already %s", requestID, req.State)
	}
	if err := s.temporal.SignalWorkflow(ctx, req.WorkflowID, "", SignalPick, nil); err != nil {
		return fmt.Errorf("signal the workflow for request %d: %w", requestID, err)
	}
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
		MissingGrace:     s.cfg.Download.MissingGrace,
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
