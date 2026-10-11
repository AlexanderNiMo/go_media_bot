package temporalx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
)

// ConnectivityModule forces the client to be constructed and checks that the
// configured namespace exists.
//
// Two separate problems, both silent without this.
//
// fx only builds what something asks for, so a graph that merely *provides*
// client.Client runs happily without ever opening a connection — the process
// starts, logs nothing, and appears healthy while Temporal has never been
// contacted. Invoking the client here is what makes "started successfully"
// mean something.
//
// The namespace check is the second: a missing namespace does not fail at
// dial time. It fails later, on the first workflow start, with a message about
// a namespace that reads like an application bug rather than a setup step that
// was skipped.
//
// This module is deliberately kept out of the graph that `mediad migrate`
// builds. Migrations need Postgres and nothing else, and a deploy that runs
// them before Temporal is up is a normal ordering, not an error.
var ConnectivityModule = fx.Module("temporal-connectivity",
	fx.Invoke(VerifyNamespace),
)

// VerifyNamespace reports whether the configured namespace is reachable.
//
// The check goes through WorkflowService because client.Client exposes no
// DescribeNamespace of its own.
func VerifyNamespace(c client.Client, cfg *config.Config, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := c.WorkflowService().DescribeNamespace(ctx,
		&workflowservice.DescribeNamespaceRequest{Namespace: cfg.Temporal.Namespace})
	switch {
	case err == nil:
		log.Info("connected to temporal",
			"host", cfg.Temporal.Host,
			"namespace", cfg.Temporal.Namespace)
		return nil

	case isNotFound(err):
		return fmt.Errorf(
			"temporal namespace %q does not exist on %s: create it with "+
				"`temporal operator namespace create -n %s`",
			cfg.Temporal.Namespace, cfg.Temporal.Host, cfg.Temporal.Namespace)

	default:
		return fmt.Errorf("describe temporal namespace %q: %w", cfg.Temporal.Namespace, err)
	}
}

func isNotFound(err error) bool {
	var notFound *serviceerror.NamespaceNotFound
	return errors.As(err, &notFound)
}
