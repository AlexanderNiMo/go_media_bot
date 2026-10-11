package request

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/alex/mediad/internal/storage"
)

// The reconciler's whole job is to tell "this row is being worked on" from
// "this row only says so", and the two mistakes it can make are asymmetric.
//
// Missing a stale row leaves it stuck, which is bad. Marking a live row stale
// turns a working request into a failed one, and the user retries something
// that never needed retrying — which is worse, and is why every check here is
// about not being trigger-happy.

// fakeTemporal answers DescribeWorkflowExecution from a table.
type fakeTemporal struct {
	client.Client

	byID map[string]*workflowservice.DescribeWorkflowExecutionResponse
	err  error
}

func (f *fakeTemporal) DescribeWorkflowExecution(
	_ context.Context, workflowID, _ string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	desc, ok := f.byID[workflowID]
	if !ok {
		return nil, serviceerror.NewNotFound("workflow not found")
	}
	return desc, nil
}

// describeWith builds a Describe response carrying one status.
func describeWith(status enums.WorkflowExecutionStatus) *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}
}

// TestIsNotFoundIsNarrow is the check that must not be widened.
//
// It would be tempting to treat any Temporal service error as "the workflow is
// gone" — they all read like plumbing — and that inverts the failure: a
// transient blip would mark every in-flight request as failed at once.
func TestIsNotFoundIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"not found", serviceerror.NewNotFound("gone"), true},
		{"wrapped not found", errWrap(serviceerror.NewNotFound("gone")), true},
		{"unavailable", serviceerror.NewUnavailable("server down"), false},
		{"internal", serviceerror.NewInternal("boom"), false},
		{"deadline", serviceerror.NewDeadlineExceeded("too slow"), false},
		{"plain", errors.New("something else"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func errWrap(err error) error { return errors.Join(errors.New("context"), err) }

// TestCheckReportsAWorkflowThatIsGone — the case the reconciler exists for.
func TestCheckReportsAWorkflowThatIsGone(t *testing.T) {
	r := &Reconciler{temporal: &fakeTemporal{byID: map[string]*workflowservice.DescribeWorkflowExecutionResponse{}}}

	reason, stale, err := r.check(context.Background(), &storage.Request{
		ID: 1, WorkflowID: "movie-603", State: storage.StateSearching,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !stale {
		t.Fatal("a request whose workflow does not exist was reported as fine")
	}
	if reason == "" {
		t.Error("no reason was given for marking the request stale")
	}
}

// TestCheckLeavesARunningWorkflowAlone — the mistake that matters.
func TestCheckLeavesARunningWorkflowAlone(t *testing.T) {
	r := &Reconciler{temporal: &fakeTemporal{byID: map[string]*workflowservice.DescribeWorkflowExecutionResponse{
		"movie-603": describeWith(enums.WORKFLOW_EXECUTION_STATUS_RUNNING),
	}}}

	_, stale, err := r.check(context.Background(), &storage.Request{
		ID: 1, WorkflowID: "movie-603", State: storage.StateDownloading,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if stale {
		t.Fatal("a running workflow was reported as gone")
	}
}

// TestCheckReportsAClosedWorkflowWithAnActiveRow.
//
// The workflow writes its outcome before it returns, so a closed execution
// with a row that still says "downloading" means that write did not happen.
func TestCheckReportsAClosedWorkflowWithAnActiveRow(t *testing.T) {
	for _, status := range []enums.WorkflowExecutionStatus{
		enums.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enums.WORKFLOW_EXECUTION_STATUS_FAILED,
		enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enums.WORKFLOW_EXECUTION_STATUS_CANCELED,
	} {
		t.Run(status.String(), func(t *testing.T) {
			r := &Reconciler{temporal: &fakeTemporal{byID: map[string]*workflowservice.DescribeWorkflowExecutionResponse{
				"movie-603": describeWith(status),
			}}}

			reason, stale, err := r.check(context.Background(), &storage.Request{
				ID: 1, WorkflowID: "movie-603", State: storage.StateDownloading,
			})
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if !stale {
				t.Fatalf("a %s workflow with an active row was reported as fine", status)
			}
			if reason == "" {
				t.Error("no reason was given")
			}
		})
	}
}

// TestCheckReportsARowWithNoWorkflow — the process died between writing the
// row and starting the execution.
func TestCheckReportsARowWithNoWorkflow(t *testing.T) {
	r := &Reconciler{temporal: &fakeTemporal{}}

	_, stale, err := r.check(context.Background(), &storage.Request{
		ID: 1, WorkflowID: "", State: storage.StatePending,
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !stale {
		t.Error("a row that never had a workflow was reported as fine")
	}
}

// TestCheckPropagatesATransientError.
//
// An outage must not be read as "everything is gone". The caller logs it and
// moves on, leaving the rows alone until the next pass.
func TestCheckPropagatesATransientError(t *testing.T) {
	r := &Reconciler{temporal: &fakeTemporal{err: serviceerror.NewUnavailable("server down")}}

	_, stale, err := r.check(context.Background(), &storage.Request{
		ID: 1, WorkflowID: "movie-603", State: storage.StateSearching,
	})
	if err == nil {
		t.Fatal("a transient failure was swallowed")
	}
	if stale {
		t.Fatal("a transient failure was read as a stale row")
	}
}

// TestReconcileLeavesFreshRowsAlone.
//
// A request row is written before its workflow is started, so there is a
// window in which a healthy request has an active state and no execution. The
// grace period is what keeps the reconciler out of it.
func TestReconcileLeavesFreshRowsAlone(t *testing.T) {
	// No database and no workflows: every row would be "stale" if the grace
	// period did not exclude them first.
	store, pool := newTestStore(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline, updated_at)
		VALUES ($1, 'movie-603', 'pending', now() + interval '2 years', now())
		RETURNING id`, mediaID).Scan(&id); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	r := &Reconciler{
		temporal: &fakeTemporal{}, // every workflow is "gone"
		store:    store,
		log:      slog.New(slog.DiscardHandler),
	}

	repaired, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d rows, want 0: a request created moments ago was marked failed", repaired)
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM requests WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if state != "pending" {
		t.Errorf("state = %q, want it untouched", state)
	}
}

// TestReconcileMarksAnOldRowWithNoWorkflow — the same row, once it is past the
// grace period.
func TestReconcileMarksAnOldRowWithNoWorkflow(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, attempt_count, search_deadline, updated_at)
		VALUES ($1, 'movie-603', 'searching', 9, now() + interval '2 years',
		        now() - interval '1 hour')
		RETURNING id`, mediaID).Scan(&id); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	r := &Reconciler{
		temporal: &fakeTemporal{},
		store:    store,
		log:      slog.New(slog.DiscardHandler),
	}

	repaired, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired %d rows, want 1", repaired)
	}

	req, err := store.GetRequest(ctx, id)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if req.State != storage.StateFailed {
		t.Errorf("state = %q, want failed", req.State)
	}
	if req.LastError == "" {
		t.Error("the reason was not recorded")
	}

	// The counters survive: a retry continues the story rather than resetting
	// it, and the attempt count is what the backoff ladder is indexed by.
	if req.AttemptCount != 9 {
		t.Errorf("attempt count = %d, want it preserved at 9", req.AttemptCount)
	}

	// And the change is in the audit trail, with the actor the schema allows
	// for exactly this.
	events, err := store.ListEvents(ctx, id, 5)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Event != "request.reconciled" || events[0].Actor != "reconciler" {
		t.Errorf("event = %q by %q", events[0].Event, events[0].Actor)
	}
	if events[0].FromState != storage.StateSearching || events[0].ToState != storage.StateFailed {
		t.Errorf("transition recorded as %q -> %q", events[0].FromState, events[0].ToState)
	}
}

// TestReconcileFreesTheRequestSoItCanBeRetried — the practical consequence,
// and the reason the reconciler is worth having.
//
// "Searching" is an active state, so without this a stuck row refuses both a
// new request and a retry, and nothing but a hand-written UPDATE can move it.
func TestReconcileFreesTheRequestSoItCanBeRetried(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	if _, err := pool.Exec(ctx, `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline, updated_at)
		VALUES ($1, 'movie-603', 'downloading', now() + interval '2 years',
		        now() - interval '1 hour')`, mediaID); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	// While the row is active, a new request for the same film is refused.
	if _, err := store.CreateRequest(ctx, storage.CreateRequestInput{
		Spec:           movieSpec(),
		WorkflowID:     "movie-603",
		NextRunAt:      time.Now(),
		SearchDeadline: time.Now().Add(time.Hour),
	}); !errors.Is(err, storage.ErrActiveRequestExists) {
		t.Fatalf("want ErrActiveRequestExists while the row is active, got %v", err)
	}

	r := &Reconciler{temporal: &fakeTemporal{}, store: store, log: slog.New(slog.DiscardHandler)}
	if _, err := r.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Once reconciled, the same insert succeeds — which is what makes the
	// request recoverable at all.
	if _, err := store.CreateRequest(ctx, storage.CreateRequestInput{
		Spec:           movieSpec(),
		WorkflowID:     "movie-603",
		NextRunAt:      time.Now(),
		SearchDeadline: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("a reconciled request is still blocking a new one: %v", err)
	}
}
