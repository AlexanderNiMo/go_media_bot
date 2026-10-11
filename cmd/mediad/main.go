// Command mediad is the single binary for the media request bot.
//
// Everything ships in one executable and the subcommand decides what it is:
//
//	mediad worker   Temporal worker: polls the task queues and does the work
//	mediad api      HTTP front door and web UI
//	mediad all      both, in one process, for a small self-hosted deployment
//	mediad migrate  apply database migrations and exit
//	mediad config   print the effective configuration with secrets redacted
//	mediad version  print build information
//
// and the commands a person types: search, add, list, status, cancel, retry and
// notifications.
//
// The CLI commands talk to Postgres and Temporal directly, so they work
// whether or not the api process is running.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/fx"
	"gopkg.in/yaml.v3"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/indexer"
	"github.com/alex/mediad/internal/logging"
	"github.com/alex/mediad/internal/plex"
	"github.com/alex/mediad/internal/qbit"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/temporalx"
	"github.com/alex/mediad/internal/tmdb"
)

// Build information, injected with -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var configFile string

	root := &cobra.Command{
		Use:   "mediad",
		Short: "Self-hosted media request bot: search, download, notify",
		Long: `mediad watches for releases of the films and series you ask for and
downloads them when a good one with Russian audio appears.

A request lives for up to two years and is checked on a backoff schedule, so
you can ask for something that does not exist yet and forget about it.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&configFile, "config", "",
		"path to config.yaml (default: ./config.yaml if it exists)")

	root.AddCommand(
		versionCmd(),
		configCmd(&configFile),
		migrateCmd(&configFile),
	)

	// The long-running modes, then the commands a person actually types.
	root.AddCommand(
		workerCmd(&configFile),
		apiCmd(&configFile),
		allCmd(&configFile),
	)

	root.AddCommand(cliCommands(&configFile)...)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Long-running modes
// ---------------------------------------------------------------------------

func workerCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the Temporal worker",
		Long: `Polls the task queues and executes workflows and activities.

The worker holds no state: kill it mid-flight and the workflows resume from
the event history when it comes back.`,
		RunE: func(*cobra.Command, []string) error {
			return runWorker(configFile)
		},
	}
}

func apiCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "api",
		Short: "Run the HTTP API and web UI",
		RunE: func(*cobra.Command, []string) error {
			return runApp(configFile)
		},
	}
}

func allCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "Run the worker and the API in one process",
		RunE: func(*cobra.Command, []string) error {
			return runWorker(configFile)
		},
	}
}

// ---------------------------------------------------------------------------
// One-shot commands
// ---------------------------------------------------------------------------

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"mediad %s (commit %s, built %s)\n", version, commit, date)
			return err
		},
	}
}

func configCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Print the effective configuration with secrets redacted",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Deliberately does not open the database or contact Temporal: the
			// point of this command is to be usable when something is broken.
			cfg, err := config.Load(config.Flags{ConfigFile: *configFile})
			if err != nil {
				return err
			}

			enc := yaml.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent(2)
			defer enc.Close()

			return enc.Encode(cfg.Redacted())
		},
	}
}

func migrateCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations and exit",
		Long: `Applies every pending migration.

Every mode applies migrations on startup anyway, so this exists for the cases
where you want the schema updated without starting a service — a deploy step,
or a check before rolling out.`,
		RunE: func(*cobra.Command, []string) error {
			return runOnce(configFile)
		},
	}
}

const (
	// startTimeout covers connecting to Postgres, applying migrations and
	// dialling Temporal. The Temporal client retries for up to a minute on its
	// own, so this has to be comfortably larger than that.
	startTimeout = 90 * time.Second

	// stopTimeout bounds the shutdown. fx runs OnStop hooks in reverse order
	// of construction, which is what makes the HTTP server drain before the
	// database pool it serves from is closed.
	stopTimeout = 30 * time.Second
)

// graph assembles the dependency graph.
//
// It is the same for every mode; what differs between worker, api and all is
// which further modules get added as those stages land.
//
// Providing *pgxpool.Pool is what applies migrations, so anything that builds
// the graph also brings the schema up to date.
func graph(configFile *string) fx.Option {
	return fx.Options(
		logging.FXLogger(),
		fx.Supply(config.Flags{ConfigFile: *configFile}),
		config.Module,
		logging.Module,
		storage.Module,
		storage.StoreModule,
		temporalx.Module,
		tmdb.Module,
		plex.Module,
		indexer.Module,
		qbit.Module,
		request.Module,
	)
}

// clientsModule groups the connectivity checks.
//
// None of these are part of `graph` because they make a service unreachable
// into a startup failure: `mediad migrate` must work with Temporal, Plex and
// qBittorrent all switched off, and a deploy that brings the schema up before
// the rest of the stack is a normal ordering rather than a mistake.
func clientsModule() fx.Option {
	return fx.Options(
		temporalx.ConnectivityModule,
		qbit.ConnectivityModule,
	)
}

// workerModule registers the workflow and its activities on the task queues.
//
// It is added only by the modes that actually execute work. The API starts
// workflows and queries them but never runs them, so giving it a worker would
// mean two processes polling the same queues and competing for the same
// activities — which works, but makes the rate limits harder to reason about
// and doubles the number of callers hitting qBittorrent.
func workerModule() fx.Option {
	return request.WorkerModule
}

// runOnce builds the graph, tears it down immediately, and returns.
//
// This is for commands whose entire effect is a side effect of construction —
// `migrate` is the only one today. It deliberately does not wait for a signal:
// doing so makes the command hang forever after the work is already done,
// which looks exactly like a migration that is stuck.
func runOnce(configFile *string) error {
	app := fx.New(graph(configFile))

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	if err := app.Start(ctx); err != nil {
		return err
	}

	return app.Stop(ctx)
}

// runWorker builds the graph with the worker attached and runs it.
func runWorker(configFile *string) error {
	return runApp(configFile, workerModule())
}

// runApp builds the graph and runs it until SIGINT/SIGTERM.
//
// Extra modules are appended after the base graph, so a mode can add the
// worker without the migration command having to know about it.
func runApp(configFile *string, extra ...fx.Option) error {
	options := []fx.Option{graph(configFile), clientsModule()}
	options = append(options, extra...)

	app := fx.New(options...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		return err
	}

	<-ctx.Done()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), stopTimeout)
	defer cancelStop()

	return app.Stop(stopCtx)
}
