package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Replaying a recorded history is the only way to catch a determinism break
// before it reaches production, and the only reason it is worth the trouble is
// what the failure looks like without it.
//
// A workflow that replays differently does not fail. The default
// WorkflowPanicPolicy is BlockWorkflow: the task fails once, and then the
// server retries it forever. The workflow never completes and never appears in
// a list of failures. With a two-year execution timeout, that means two years
// of silence — and the request that was supposed to be looking for a film just
// stops, without anyone being told.
//
// The test environment cannot catch this. It runs the workflow fresh and never
// replays anything, so it will happily pass on code that cannot replay. Only a
// real history can answer the question.
//
// The corpus is captured from real runs and committed; see
// scripts/capture-histories.sh. It is small on purpose: one history per
// decision point that has ever existed, rather than a large sample that
// nothing reads.

const (
	historiesDir = "testdata/histories"

	// requireCorpusEnv turns an empty corpus from a skip into a failure.
	//
	// It exists so that CI cannot quietly lose this coverage: a fresh clone
	// legitimately has no histories captured yet, while a repository that is
	// supposed to have them and does not is a problem worth failing over.
	requireCorpusEnv = "MEDIAD_REQUIRE_REPLAY"
)

// TestReplayGoldenHistories replays every recorded history against the current
// workflow code.
func TestReplayGoldenHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(historiesDir, "*.json"))
	if err != nil {
		t.Fatalf("look for recorded histories: %v", err)
	}

	if len(files) == 0 {
		msg := "no recorded histories in " + historiesDir +
			"; capture some with scripts/capture-histories.sh"
		if os.Getenv(requireCorpusEnv) != "" {
			t.Fatal(msg + " (and " + requireCorpusEnv + " is set, so this is a failure rather than a skip)")
		}
		t.Skip(msg)
	}

	replayer := worker.NewWorkflowReplayer()

	// Registered with the pinned type names, not with RegisterWorkflow.
	//
	// RegisterWorkflow names a workflow after its Go function, and these are
	// deliberately not that: the function is `Workflow` and the type is
	// `RequestWorkflow`. A history names the type it ran under, so registering
	// the function name would make every replay fail with "unknown workflow
	// type" — a failure that looks like a determinism problem and is not.
	//
	// Every type that appears in any history has to be registered, or the
	// replay fails before it reaches the code it is meant to check.
	replayer.RegisterWorkflowWithOptions(Workflow,
		workflow.RegisterOptions{Name: WorkflowType})
	replayer.RegisterWorkflowWithOptions(DownloadWatcherWorkflow,
		workflow.RegisterOptions{Name: DownloadWatcherWorkflowType})

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")

		t.Run(name, func(t *testing.T) {
			// A nil logger: the replayer's own logging is noise here, and the
			// returned error carries the mismatch.
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, file); err != nil {
				t.Fatalf("history %s no longer replays:\n%v\n\n"+
					"This means the workflow code changed in a way that alters what it "+
					"does for an execution that already happened. Every request currently "+
					"running will wedge on its next workflow task: it will retry forever, "+
					"never complete, and never appear as a failure. See the README section "+
					"on versioning for what to do instead.", name, err)
			}
		})
	}
}

// TestReplayCorpusCoversEveryWorkflowType guards against a corpus that exists
// but no longer covers what it is supposed to.
//
// A history for a workflow type that was renamed or removed would leave that
// type unprotected, and the corpus would look healthy while covering less than
// it did.
func TestReplayCorpusCoversEveryWorkflowType(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(historiesDir, "*.json"))
	if err != nil {
		t.Fatalf("look for recorded histories: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no recorded histories yet")
	}

	want := map[string]bool{
		WorkflowType:                false,
		DownloadWatcherWorkflowType: false,
	}

	for _, file := range files {
		// Read as bytes rather than parsing: the workflow type appears in the
		// history's WorkflowExecutionStarted event, and a substring check is
		// enough to know which type a file covers without pulling in the
		// history proto types for one question.
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for name := range want {
			if strings.Contains(string(data), name) {
				want[name] = true
			}
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("no recorded history covers %s; a change to it would go undetected "+
				"until it wedged every running execution", name)
		}
	}
}
