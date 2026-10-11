// Package temporalx holds the shared Temporal wiring: the client, the task
// queue names, and every string that forms part of the contract with the event
// history.
package temporalx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/client"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
)

// Module wires the Temporal client into the fx graph.
var Module = fx.Module("temporal", fx.Provide(NewClient))

// Task queues. Each one exists to carry its own concurrency and rate limits;
// see the worker wiring for the numbers.
//
// A workflow and its activities must agree on a queue. A mismatch produces no
// error anywhere — the activity is scheduled and simply never picked up, and
// the workflow waits forever. That is why every name lives here rather than
// being written as a literal at each call site.
const (
	// TaskQueueWorkflows runs the workflows themselves.
	TaskQueueWorkflows = "mediad"
	// TaskQueueIndexer carries SearchIndexer and is rate limited server-side,
	// because Prowlarr fans out to every configured tracker and an unthrottled
	// burst gets the IP banned rather than merely slowed down.
	TaskQueueIndexer = "indexer-api"
	// TaskQueueQBittorrent carries AddTorrent and DeleteTorrent.
	TaskQueueQBittorrent = "qbittorrent"
	// TaskQueueQbitWatch carries the long-lived download watcher activity.
	TaskQueueQbitWatch = "qbit-watch"
	// TaskQueuePlex carries the Plex library lookups.
	TaskQueuePlex = "plex"
	// TaskQueueNotify carries user notifications.
	TaskQueueNotify = "notify"
	// TaskQueueDB carries projection writes and other fast database work.
	TaskQueueDB = "db"
)

// Workflow type names.
//
// The default type name is the Go function name, which makes a rename a
// breaking change for every workflow already in flight — and these run for two
// years. Pinning the names decouples the two.
const (
	// RequestWorkflowType is the per-media search-and-download workflow.
	RequestWorkflowType = "RequestWorkflow"
	// DownloadWatcherWorkflowType is the singleton that polls qBittorrent.
	DownloadWatcherWorkflowType = "DownloadWatcherWorkflow"
)

// Signal and update names.
const (
	// UpdateCancel requests cancellation and reports whether compensation ran.
	UpdateCancel = "cancel"
	// UpdateRetry forces a failed request back into the search loop.
	UpdateRetry = "retry"
)

// Query names.
const (
	// QueryState reports the workflow's own view of a request.
	QueryState = "state"
)

// NewClient dials the Temporal server and closes the connection on shutdown.
//
// The client is safe for concurrent use and holds a gRPC connection, so one
// instance is shared across the process.
//
// Dialling is retried. client.Dial performs an eager connectivity check and
// returns an error rather than blocking, so without this a Temporal container
// that is still starting — or a Postgres restart mid-boot — would crash-loop
// the process. The retry budget is generous because the alternative is a
// restart loop that looks like a broken deployment.
func NewClient(lc fx.Lifecycle, cfg *config.Config, log *slog.Logger) (client.Client, error) {
	opts := client.Options{
		HostPort:  cfg.Temporal.Host,
		Namespace: cfg.Temporal.Namespace,
	}
	// The Temporal logger is separate from slog. Wiring it through keeps
	// workflow and activity logs in the same stream as everything else.
	opts.Logger = newTemporalLogger(log)

	c, err := dialWithRetry(context.Background(), opts, log)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			c.Close()
			return nil
		},
	})

	return c, nil
}

const (
	dialAttempts = 30
	dialInterval = 2 * time.Second
)

func dialWithRetry(ctx context.Context, opts client.Options, log *slog.Logger) (client.Client, error) {
	var lastErr error
	for attempt := 1; attempt <= dialAttempts; attempt++ {
		c, err := client.Dial(opts)
		if err == nil {
			if attempt > 1 {
				log.Info("connected to temporal", "host", opts.HostPort, "attempt", attempt)
			}
			return c, nil
		}
		lastErr = err

		if attempt == dialAttempts {
			break
		}
		log.Warn("temporal not reachable, retrying",
			"host", opts.HostPort, "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(dialInterval):
		}
	}

	return nil, fmt.Errorf("connect to temporal at %s after %d attempts: %w",
		opts.HostPort, dialAttempts, lastErr)
}
