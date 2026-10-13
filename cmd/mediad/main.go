// Command mediad is the single binary for the media request bot.
//
// Everything ships in one executable and the subcommand decides what it is:
//
//	mediad worker   Temporal worker: polls the task queues and does the work
//	mediad api      HTTP front door and web UI
//	mediad all      both, in one process, for a small self-hosted deployment
//	mediad bot      the Telegram bot only
//	mediad migrate  apply database migrations and exit
//	mediad config   print the effective configuration with secrets redacted
//	mediad health   exit non-zero if Postgres or Temporal is unreachable
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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"go.temporal.io/sdk/client"
	"go.uber.org/fx"
	"gopkg.in/yaml.v3"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/indexer"
	"github.com/alex/mediad/internal/logging"
	"github.com/alex/mediad/internal/plex"
	"github.com/alex/mediad/internal/qbit"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/telegram"
	"github.com/alex/mediad/internal/temporalx"
	"github.com/alex/mediad/internal/tmdb"
	"github.com/alex/mediad/internal/web"
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
		healthCmd(&configFile),
		configCmd(&configFile),
		migrateCmd(&configFile),
	)

	// The long-running modes, then the commands a person actually types.
	root.AddCommand(
		workerCmd(&configFile),
		apiCmd(&configFile),
		botCmd(&configFile),
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
			return runAPI(configFile)
		},
	}
}

func botCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "bot",
		Short: "Run the Telegram bot",
		Long: `Polls Telegram for messages and delivers notifications.

It is separate from worker and api because it is the one piece that talks to
the outside world, and running it on its own means a deployment can keep the
bot off the machine that holds the library.

Without it, notifications stay in the database and are delivered whenever it
starts: the table is the queue, so nothing is lost in the meantime.`,
		RunE: func(*cobra.Command, []string) error {
			return runBot(configFile)
		},
	}
}

func allCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "Run the worker and the API in one process",
		RunE: func(*cobra.Command, []string) error {
			return runAll(configFile)
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

// healthCmd reports whether the service can do its job.
//
// It exists for the container healthcheck, and it is deliberately a separate
// command rather than something the binary does on its own: a probe has to be
// a fresh process that talks to the running service, because one that merely
// runs inside the container succeeds while the main process crash-loops. That
// is a health signal that lies, which is worse than none.
//
// It checks Postgres and Temporal — the two dependencies without which nothing
// works — and nothing else. Plex, the indexer and qBittorrent are all allowed
// to be down: the worker keeps running and retries, and a restart caused by a
// torrent client being briefly unavailable would turn a blip into an outage.
func healthCmd(configFile *string) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check that the database and the workflow engine are reachable",
		Long: `Exits non-zero if a dependency the service cannot work without is unreachable.

Postgres and Temporal are checked. Plex, the indexer and qBittorrent are not:
those are expected to come and go, and the worker retries against them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(config.Flags{ConfigFile: *configFile})
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			// Postgres: a pool that refuses to open, or opens onto a database
			// that will not answer a ping, is not one the service can use.
			pool, err := pgxpool.New(ctx, cfg.Postgres.DSN)
			if err != nil {
				return fmt.Errorf("postgres: %w", err)
			}
			defer pool.Close()

			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres: %w", err)
			}

			// Temporal: the namespace has to exist, not merely the server.
			// Those are different conditions, and the second is what workflow
			// starts actually need.
			//
			// Dialled directly rather than through the fx graph, because
			// client.Dial is what does the eager connectivity check and returns
			// an error; the graph's own client retries for a minute, which a
			// probe with a 5-second budget cannot afford.
			tc, err := client.Dial(client.Options{
				HostPort:  cfg.Temporal.Host,
				Namespace: cfg.Temporal.Namespace,
			})
			if err != nil {
				return fmt.Errorf("temporal: %w", err)
			}
			defer tc.Close()

			if err := temporalx.CheckNamespace(ctx, tc, cfg); err != nil {
				return fmt.Errorf("temporal: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second,
		"how long to wait for the dependencies")

	return cmd
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

// The connectivity checks, split by who actually needs what.
//
// None of them are part of `graph`, because each one turns an unreachable
// dependency into a startup failure: `mediad migrate` must work with Temporal
// and qBittorrent both switched off, and bringing the schema up before the
// rest of the stack is a normal deploy order rather than a mistake.
//
// They are split rather than grouped because the grouping is what a process
// *uses*, and getting it wrong is not a small thing: `mediad api` refuses to
// start when the torrent client is down even though it never talks to it, so
// the interface you would check to find out what is going on is unavailable
// exactly when something is wrong.

// temporalConnectivity forces the client to be built and the namespace to
// exist. Needed by anything that starts or queries a workflow.
func temporalConnectivity() fx.Option {
	return temporalx.ConnectivityModule
}

// qbitConnectivity logs in at startup, which is what validates the credentials
// and the version before a torrent is handed over.
//
// Only the worker: adding and removing torrents is its job, and a login
// attempt from a process that will never add one is a session it does not
// need against a client that bans IPs after repeated failures.
func qbitConnectivity() fx.Option {
	return qbit.ConnectivityModule
}

// webModule adds the HTTP interface.
//
// It is a module of its own rather than part of the base graph because the CLI
// commands must not start an HTTP listener: `mediad list` opening a port would
// be surprising, and on a host where `mediad api` is already running it would
// fail to bind and take the command down with it.
func webModule() fx.Option {
	return fx.Options(
		web.Module,
		fx.Invoke(func(s *web.Server) { s.SetVersion(version) }),
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
//
// The reconciler comes with it: it needs a Temporal client that can describe
// executions, and the worker is the process already expected to be running
// continuously.
func runWorker(configFile *string) error {
	return runApp(configFile,
		temporalConnectivity(), qbitConnectivity(),
		workerModule(), request.ReconcilerModule)
}

// runAPI builds the graph with the HTTP interface attached.
//
// Temporal is required and qBittorrent is not: the interface starts and
// cancels workflows, and reads progress from the projection the worker writes.
func runAPI(configFile *string) error {
	return runApp(configFile, temporalConnectivity(), webModule())
}

// runBot builds the graph with the Telegram bot attached.
//
// It refuses to start when the bot is not configured, and that check is worth
// the extra config load. Without it the command builds the whole graph, finds
// nothing to poll, and waits for a signal — a process that does nothing at all
// while looking exactly like one that works, which is the worst way for a
// misconfiguration to present itself.
//
// `mediad all` has no such check: a disabled bot there is normal, and the
// worker and the interface still have work to do.
func runBot(configFile *string) error {
	cfg, err := config.Load(config.Flags{ConfigFile: *configFile})
	if err != nil {
		return err
	}

	switch {
	case !cfg.Telegram.Enabled:
		return fmt.Errorf("the telegram bot is disabled; set telegram.enabled " +
			"(MEDIAD_TELEGRAM_ENABLED=true) and telegram.token, or use `mediad all` " +
			"to run the worker and the interface without it")
	case strings.TrimSpace(cfg.Telegram.Token) == "":
		return fmt.Errorf("telegram.token is empty; get one from @BotFather and set " +
			"MEDIAD_TELEGRAM_TOKEN")
	}

	return runApp(configFile, temporalConnectivity(), telegram.Module)
}

// runAll builds the graph with everything, for a small self-hosted deployment.
func runAll(configFile *string) error {
	return runApp(configFile,
		temporalConnectivity(), qbitConnectivity(),
		workerModule(), request.ReconcilerModule, webModule(), telegram.Module)
}

// runApp builds the graph and runs it until SIGINT/SIGTERM.
//
// Extra modules are appended after the base graph, so a mode can add the
// worker without the migration command having to know about it.
func runApp(configFile *string, extra ...fx.Option) error {
	options := append([]fx.Option{graph(configFile)}, extra...)

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
