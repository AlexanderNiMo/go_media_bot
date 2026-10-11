package request

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

	"github.com/alex/mediad/internal/config"
)

// The two things this file tests have the same shape of failure: nothing goes
// wrong, and nothing ever happens. A workflow started under a type name no
// worker registered, and a start that attaches to an execution already
// running, both report success and both leave a request waiting forever. There
// is no error to find in a log, which is why they are pinned here.

// fakeWorkflowClient records what the service asks the engine to do.
type fakeWorkflowClient struct {
	// started is the second argument of ExecuteWorkflow exactly as it arrived:
	// a type name, or a function that the SDK would have turned into one.
	started     any
	startedOpts client.StartWorkflowOptions
	startedArgs []any
	startErr    error
}

// startedType is the type name the engine would derive, following the SDK's
// own rule: a string is used as it is, and a function means its Go name.
func (f *fakeWorkflowClient) startedType() string {
	switch v := f.started.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprintf("%T (a function, not a type name)", v)
	}
}

func (f *fakeWorkflowClient) ExecuteWorkflow(
	_ context.Context,
	options client.StartWorkflowOptions,
	workflow any,
	args ...any,
) (client.WorkflowRun, error) {
	f.started = workflow
	f.startedOpts = options
	f.startedArgs = args
	return nil, f.startErr
}

func (f *fakeWorkflowClient) CancelWorkflow(context.Context, string, string) error { return nil }

func (f *fakeWorkflowClient) QueryWorkflow(
	context.Context, string, string, string, ...any,
) (converter.EncodedValue, error) {
	return nil, nil
}

// TestWorkflowIsStartedByItsPinnedTypeName is the guard for the defect that
// makes the whole project do nothing.
//
// ExecuteWorkflow takes either a function or a type name. Given a function it
// uses the function's own Go name and ignores the alias the worker registered
// it under, because it consults the *client's* registry and aliases live on
// workers. The function here is called Workflow, so passing it starts
// executions of type "Workflow" while every worker registers "RequestWorkflow".
//
// Nothing reports that. The execution is accepted, sits in the queue, and no
// worker can claim it — so a request created through any interface would wait
// out its full two-year timeout without a single search.
func TestWorkflowIsStartedByItsPinnedTypeName(t *testing.T) {
	fake := &fakeWorkflowClient{}
	svc := &Service{temporal: fake}

	if _, err := svc.startRun(context.Background(), client.StartWorkflowOptions{ID: "movie-603"},
		Input{RequestID: 1}); err != nil {
		t.Fatalf("startRun: %v", err)
	}

	if got := fake.startedType(); got != WorkflowType {
		t.Fatalf("the workflow was started as %s, want the type name %q.\n\n"+
			"A workflow whose type no worker registered is not an error anywhere: "+
			"it is scheduled, never claimed, and waits silently. The worker "+
			"registers %q, so starting anything else means no request ever runs.",
			got, WorkflowType, WorkflowType)
	}
}

// TestStartedTypeMatchesTheRecordedHistories ties the name the service starts
// to the name real executions ran under.
//
// The constants could drift from the corpus — a rename would leave a starter
// and a worker agreeing with each other and disagreeing with every execution
// already in flight — and the corpus is the only artefact that knows what
// those were called.
func TestStartedTypeMatchesTheRecordedHistories(t *testing.T) {
	if WorkflowType != "RequestWorkflow" {
		t.Fatalf("WorkflowType = %q, but the recorded histories and every running "+
			"execution use \"RequestWorkflow\"; changing this constant renames the "+
			"workflow for new executions only, and orphans the ones in flight",
			WorkflowType)
	}
	if DownloadWatcherWorkflowType != "DownloadWatcherWorkflow" {
		t.Errorf("DownloadWatcherWorkflowType = %q, want the name the recorded "+
			"watcher histories use", DownloadWatcherWorkflowType)
	}
}

// TestRetryRefusesToAttachToARunningWorkflow pins the pair of options the retry
// path needs, because either one without the other is a no-op.
//
// ConflictPolicy FAIL is what makes the *server* refuse to reuse the id of a
// running execution. The Go client then takes that refusal and turns it into a
// handle to the running execution with a nil error, unless
// WorkflowExecutionErrorWhenAlreadyStarted is set — so FAIL on its own returns
// success, and the retry would reset the projection while the old execution
// carried on with its old state.
func TestRetryRefusesToAttachToARunningWorkflow(t *testing.T) {
	svc := &Service{cfg: &config.Config{}}
	opts := svc.retryOptions("movie-603")

	if opts.WorkflowIDConflictPolicy != enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL {
		t.Errorf("retry conflict policy = %v, want FAIL: the server must refuse "+
			"to attach to a running execution", opts.WorkflowIDConflictPolicy)
	}
	if !opts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Error("retry does not raise on an already-started workflow, so the " +
			"server's refusal would be swallowed and the retry would report " +
			"success while doing nothing")
	}

	// A closed execution must still be reusable, or a request that failed
	// could never be retried within the two years its id is reserved.
	if opts.WorkflowIDReusePolicy != enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE {
		t.Errorf("reuse policy = %v, want ALLOW_DUPLICATE", opts.WorkflowIDReusePolicy)
	}
}

// TestStartRequestAttachesOnPurpose — the opposite of the retry, and worth
// stating because the two are one field apart.
//
// A request for something already being fetched should attach to it: that is
// the deduplication rule, and a second download of the same film is the thing
// it exists to prevent.
func TestStartRequestAttachesOnPurpose(t *testing.T) {
	cfg := &config.Config{}
	// Two years, which is the product's search horizon. Set rather than left
	// to the fallback, so that this asserts the backstop is derived from the
	// configured deadline instead of coinciding with a constant.
	cfg.Search.SearchDeadline = 17520 * time.Hour

	svc := &Service{cfg: cfg}
	opts := svc.startOptions("movie-603")

	if opts.WorkflowIDConflictPolicy != enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING {
		t.Errorf("start conflict policy = %v, want USE_EXISTING", opts.WorkflowIDConflictPolicy)
	}

	// Three days beyond the product's two-year deadline: a release found on the
	// last day still needs its download window, and this timeout kills the
	// workflow rather than failing it.
	want := cfg.Search.SearchDeadline + 72*time.Hour
	if opts.WorkflowExecutionTimeout != want {
		t.Errorf("execution timeout = %v, want %v", opts.WorkflowExecutionTimeout, want)
	}
}

// TestServiceRecognisesAnAlreadyStartedWorkflow — the error the engine returns
// when a retry loses the race is turned into something a person can act on,
// because it means the projection and the engine disagree.
func TestIsAlreadyStartedError(t *testing.T) {
	if temporal.IsWorkflowExecutionAlreadyStartedError(context.Canceled) {
		t.Error("an unrelated error was read as an already-started workflow")
	}
}
