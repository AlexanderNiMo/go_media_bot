package request

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/storage"
)

// ReconcilerModule starts the loop that catches a projection drifting away
// from the workflow engine.
var ReconcilerModule = fx.Module("reconciler",
	fx.Invoke(registerReconciler),
)

const (
	// reconcileInterval is how often the check runs.
	//
	// Slow on purpose. It exists for the state that is already wrong and is
	// not getting worse: a request sitting in "searching" with nothing running
	// has been in that state for minutes by the time it is noticed, and being
	// told about it a few minutes later changes nothing.
	reconcileInterval = 10 * time.Minute

	// reconcileGrace is how new a row may be before it is left alone.
	//
	// Essential, not defensive. A request row is written before its workflow
	// is started, so there is a window — milliseconds wide — in which a
	// perfectly healthy request has an active state and no execution. Without
	// a grace period the reconciler would eventually land in that window and
	// mark somebody's brand-new request as failed.
	//
	// The window is measured against updated_at, which the workflow and the
	// download watcher both touch, so a row that is being worked on is never
	// old enough to be considered.
	reconcileGrace = 15 * time.Minute

	// reconcileBatch caps how many rows one pass looks at, so that a large
	// backlog cannot turn into an unbounded burst of Describe calls.
	reconcileBatch = 200
)

// Reconciler compares the projection against the workflow engine.
//
// It exists because the two can disagree, and because the disagreement is
// silent. A workflow that is terminated, or that was never claimable because
// its task queue had no worker for its type, leaves a row reading "searching"
// with nothing behind it — and since "searching" is an active state, both a
// new request and a retry refuse to touch it. Without this the row is stuck
// until somebody reads the database by hand.
type Reconciler struct {
	temporal client.Client
	store    *storage.Store
	log      *slog.Logger
}

// NewReconciler builds the reconciler.
func NewReconciler(c client.Client, store *storage.Store, log *slog.Logger) *Reconciler {
	return &Reconciler{temporal: c, store: store, log: log.With("component", "reconciler")}
}

// registerReconciler runs the loop for the life of the process.
func registerReconciler(lc fx.Lifecycle, c client.Client, cfg *config.Config,
	store *storage.Store, log *slog.Logger) {
	// Only where a worker lives. The api process would also work — the check
	// is idempotent, so two of them would be harmless — but it is the worker
	// that is already expected to be running continuously, and giving the api
	// a Temporal client that can describe executions is a dependency it does
	// not otherwise need.
	_ = cfg

	rec := NewReconciler(c, store, log)

	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go rec.run(ctx)
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			return nil
		},
	})
}

func (r *Reconciler) run(ctx context.Context) {
	// A first pass shortly after startup rather than one interval later, since
	// a restart is exactly when rows are most likely to be stale: a workflow
	// whose worker died mid-flight is not running any more.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if n, err := r.Reconcile(ctx); err != nil {
				r.log.Error("reconciliation failed", "error", err)
			} else if n > 0 {
				r.log.Warn("marked requests as stale: their workflow is not running",
					"count", n)
			}
			timer.Reset(reconcileInterval)
		}
	}
}

// Reconcile runs one pass and returns how many rows it repaired.
func (r *Reconciler) Reconcile(ctx context.Context) (int, error) {
	active, err := r.store.ListRequests(ctx, storage.RequestFilter{
		States: storage.ActiveStates(),
		Limit:  reconcileBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("list active requests: %w", err)
	}

	cutoff := time.Now().Add(-reconcileGrace)
	repaired := 0

	for _, req := range active {
		if req.UpdatedAt.After(cutoff) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return repaired, err
		}

		reason, stale, err := r.check(ctx, req)
		if err != nil {
			// One unreachable execution must not stop the pass: the rest of
			// the list still needs checking.
			r.log.Warn("could not check a request",
				"request", req.ID, "workflow", req.WorkflowID, "error", err)
			continue
		}
		if !stale {
			continue
		}

		if err := r.markStale(ctx, req, reason); err != nil {
			r.log.Error("could not mark a stale request", "request", req.ID, "error", err)
			continue
		}
		repaired++
	}

	return repaired, nil
}

// check reports whether a request's workflow is gone, and why.
func (r *Reconciler) check(ctx context.Context, req *storage.Request) (string, bool, error) {
	if req.WorkflowID == "" {
		// A row from before the workflow was started. The grace period already
		// covers the ordinary case; this is the one where the process died in
		// between and the start never happened.
		return "the workflow was never started", true, nil
	}

	desc, err := r.temporal.DescribeWorkflowExecution(ctx, req.WorkflowID, "")
	if err != nil {
		if isNotFound(err) {
			return "the workflow no longer exists", true, nil
		}
		return "", false, err
	}

	info := desc.GetWorkflowExecutionInfo()
	if info == nil {
		return "", false, nil
	}

	switch info.GetStatus() {
	case enums.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return "", false, nil
	default:
		// Closed, but the row still says the request is being worked on. The
		// workflow writes its outcome before it returns, so a closed execution
		// with an active row means that write did not happen — the execution
		// was terminated, or the worker died between the last activity and the
		// return.
		return fmt.Sprintf("the workflow ended as %s without recording an outcome",
			strings.ToLower(info.GetStatus().String())), true, nil
	}
}

// markStale records what was found and returns the row to a state somebody can
// act on.
//
// Failed rather than canceled: it is not a decision anybody made, and a user
// who sees "failed" knows to retry, where a silent "canceled" looks like
// something they did.
func (r *Reconciler) markStale(ctx context.Context, req *storage.Request, reason string) error {
	r.log.Warn("a request's workflow is not running",
		"request", req.ID,
		"workflow", req.WorkflowID,
		"state", req.State,
		"reason", reason)

	return r.store.SaveRequestState(ctx, storage.RequestState{
		ID:          req.ID,
		State:       storage.StateFailed,
		Attempt:     req.AttemptCount,
		ErrorStreak: req.ErrorStreak,
		StallCount:  req.StallCount,
		LastError:   reason,
	}, storage.Event{
		RequestID: req.ID,
		Event:     "request.reconciled",
		FromState: req.State,
		ToState:   storage.StateFailed,
		Actor:     "reconciler",
		Detail: map[string]any{
			"workflow_id": req.WorkflowID,
			"reason":      reason,
		},
	})
}

// isNotFound reports whether the error means the execution does not exist.
//
// The type, not the message, and not "any service error". Getting this wrong
// is the dangerous direction: a transient failure read as "the workflow is
// gone" marks a perfectly healthy request as failed, and the user retries
// something that was working. So the check is narrow — one concrete type —
// and anything else propagates and is logged.
//
// A closed execution whose history has aged past retention reports the same
// way, which is the right answer: as far as this can tell, there is nothing
// running and nothing to resume.
func isNotFound(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}
