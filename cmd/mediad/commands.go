package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/prompt"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
)

// cliCommands is the user-facing half of the binary: what you run to ask for
// something and to find out what happened to it.
//
// They all talk to Postgres and Temporal directly, so they work without the api
// process running — which is the point, because the moment you most need to ask
// what a request is doing is the moment the interface is broken.
func cliCommands(configFile *string) []*cobra.Command {
	return []*cobra.Command{
		searchCmd(configFile),
		addCmd(configFile),
		listCmd(configFile),
		statusCmd(configFile),
		cancelCmd(configFile),
		retryCmd(configFile),
		notificationsCmd(configFile),
	}
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

func searchCmd(configFile *string) *cobra.Command {
	var (
		pick   int
		season string
		user   string
		force  bool
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search TMDB for a title and request it",
		Long: `Searches TMDB, asks which of the matches you meant, and starts a request.

A series needs a season: this project has no "the whole series" request, so
each season is requested — and downloaded — on its own.

Starting a request is not the same as downloading it. The workflow first asks
Plex whether the library already has it, and tells you if it does.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wanted, err := parseSeason(season)
			if err != nil {
				return err
			}

			var deps writeDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				return runSearch(cmd, ctx, &deps, searchOptions{
					Query:  args[0],
					Pick:   pick,
					Season: wanted,
				}, user, force, asJSON)
			})
		},
	}

	// The default is printed by --help, so it is written to read as part of
	// the description rather than as an internal sentinel leaking out.
	cmd.Flags().IntVar(&pick, "pick", 0, "take the Nth match without asking (0 asks)")
	cmd.Flags().StringVar(&season, "season", "", "season to request for a series (omit to choose)")
	cmd.Flags().StringVar(&user, "user", "", "user to attribute the request to (default: $USER)")
	cmd.Flags().BoolVar(&force, "force", false, "start a new request even if one is already running")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the request as JSON")

	return cmd
}

func runSearch(
	cmd *cobra.Command,
	ctx context.Context,
	deps *writeDeps,
	opts searchOptions,
	user string,
	force, asJSON bool,
) error {
	spec, err := resolveSearch(ctx, deps.TMDB, prompterFor(cmd), opts)
	if err != nil {
		return err
	}

	return startRequest(cmd, ctx, deps, spec, user, force, asJSON)
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

func addCmd(configFile *string) *cobra.Command {
	var (
		season string
		user   string
		force  bool
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "add <movie|tv> <tmdb-id>",
		Short: "Request a title by its TMDB id",
		Long: `Requests a title without searching for it first.

This is the form to use from a script: it never asks a question. The TMDB id is
in the URL of the title's page on themoviedb.org.`,
		Example: `  mediad add movie 603
  mediad add tv 1399 --season 2`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := ensureMediaType(args[0])
			if err != nil {
				return err
			}

			var tmdbID int64
			if _, err := fmt.Sscanf(args[1], "%d", &tmdbID); err != nil || tmdbID <= 0 {
				return fmt.Errorf("%q is not a TMDB id: expected a number", args[1])
			}

			ref := domain.MovieRef(tmdbID)
			if kind == domain.MediaTypeTV {
				ref = domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: tmdbID}
			}

			wanted, err := parseSeason(season)
			if err != nil {
				return err
			}

			var deps writeDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				spec, err := resolveByID(ctx, deps.TMDB, ref, prompterFor(cmd), wanted)
				if err != nil {
					return err
				}
				return startRequest(cmd, ctx, &deps, spec, user, force, asJSON)
			})
		},
	}

	cmd.Flags().StringVar(&season, "season", "", "season to request for a series (omit to choose)")
	cmd.Flags().StringVar(&user, "user", "", "user to attribute the request to (default: $USER)")
	cmd.Flags().BoolVar(&force, "force", false, "start a new request even if one is already running")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the request as JSON")

	return cmd
}

// startRequest is the shared tail of search and add.
func startRequest(
	cmd *cobra.Command,
	ctx context.Context,
	deps *writeDeps,
	spec domain.MediaSpec,
	user string,
	force, asJSON bool,
) error {
	if user == "" {
		user = defaultCLIUser()
	}

	// Created on demand, so that a request made from the terminal belongs to
	// somebody and produces notifications that can be read back. The same
	// account is what the bot and the web UI reach through their own
	// identities.
	account, err := deps.Store.EnsureUserByCLI(ctx, user)
	if err != nil {
		return err
	}

	res, err := deps.Req.StartRequest(ctx, request.StartInput{
		Spec:   spec,
		UserID: account.ID,
		Force:  force,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if asJSON {
		return writeJSON(out, requestToJSON(res.Request))
	}

	if res.Reused {
		fmt.Fprintf(out, "Already being fetched: request %d (%s) — %s\n",
			res.Request.ID, res.Request.State, requestTitle(res.Request))
		fmt.Fprintf(out, "Added %s to its watchers; you will be told when it finishes.\n", user)
		return nil
	}

	fmt.Fprintf(out, "Started request %d — %s  [%s]\n",
		res.Request.ID, requestTitle(res.Request), res.Request.WorkflowID)
	fmt.Fprintf(out, "  checking whether Plex already has it;\n"+
		"  `mediad status %d` will tell you what it found\n", res.Request.ID)

	return nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func listCmd(configFile *string) *cobra.Command {
	var (
		states []string
		all    bool
		limit  int
		user   string
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List requests",
		Long: `Lists requests, newest first.

By default only the ones still being worked on, which is the list somebody
actually wants; --all adds everything that has finished.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var deps readDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				filter := storage.RequestFilter{Limit: limit}

				switch {
				case all:
				case len(states) > 0:
					for _, s := range states {
						state := storage.State(s)
						if !state.Known() {
							return fmt.Errorf("unknown state %q; the states are %s",
								s, sortedStates(storage.AllStates()))
						}
						filter.States = append(filter.States, state)
					}
				default:
					filter.States = storage.ActiveStates()
				}

				if user != "" {
					account, err := deps.Store.FindUserByCLI(ctx, user)
					if err != nil {
						return err
					}
					filter.WatcherUserID = account.ID
				}

				requests, err := deps.Store.ListRequests(ctx, filter)
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()

				if asJSON {
					rows := make([]requestJSON, 0, len(requests))
					for _, r := range requests {
						rows = append(rows, requestToJSON(r))
					}
					return writeJSON(out, rows)
				}

				if len(requests) == 0 {
					fmt.Fprintln(out, "no requests")
					return nil
				}

				now := time.Now()
				rows := [][]string{{"ID", "STATE", "TITLE", "WHEN"}}
				for _, r := range requests {
					rows = append(rows, []string{
						fmt.Sprint(r.ID),
						string(r.State),
						truncate(requestTitle(r), 44),
						truncate(requestWhen(r, now), 52),
					})
				}

				for _, line := range padColumns(rows, 2) {
					fmt.Fprintln(out, line)
				}
				return nil
			})
		},
	}

	cmd.Flags().StringSliceVar(&states, "state", nil,
		"only these states: "+sortedStates(storage.AllStates()))
	cmd.Flags().BoolVar(&all, "all", false, "include finished requests")
	cmd.Flags().IntVar(&limit, "limit", 50, "how many to show")
	cmd.Flags().StringVar(&user, "user", "", "only requests this user is watching")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func statusCmd(configFile *string) *cobra.Command {
	var (
		events int
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "status <id|workflow-id>",
		Short: "Show one request and its history",
		Long: `Shows everything known about one request, and what it has done recently.

The identifier may be the number from ` + "`mediad list`" + ` or the workflow id,
which is what the Temporal UI shows.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var deps readDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				req, err := findRequest(ctx, deps.Store, args[0])
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()

				if asJSON {
					payload := struct {
						requestJSON
						Events []eventJSON `json:"events,omitempty"`
					}{requestToJSON(req), nil}

					if events > 0 {
						rows, err := deps.Store.ListEvents(ctx, req.ID, events)
						if err != nil {
							return err
						}
						for _, e := range rows {
							payload.Events = append(payload.Events, eventToJSON(e))
						}
					}
					return writeJSON(out, payload)
				}

				writeStatus(out, req)

				if events <= 0 {
					return nil
				}

				rows, err := deps.Store.ListEvents(ctx, req.ID, events)
				if err != nil {
					return err
				}
				writeEvents(out, rows)

				return nil
			})
		},
	}

	cmd.Flags().IntVar(&events, "events", 20, "how many audit events to show (0 for none)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

func writeStatus(out io.Writer, r *storage.Request) {
	now := time.Now()

	fmt.Fprintf(out, "Request %d — %s  [%s]\n", r.ID, requestTitle(r), r.WorkflowID)
	field := func(label, value string) {
		if value != "" {
			fmt.Fprintf(out, "  %-14s %s\n", label, value)
		}
	}

	field("state", string(r.State))
	field("plex", describePlex(r))
	field("search", describeSearch(r, now))

	if selected := describeSelected(r); selected != "" {
		field("release", selected)
	}
	if r.State == storage.StateDownloading {
		field("progress", requestWhen(r, now))
	}
	if r.QbitHash != "" {
		field("torrent", r.QbitHash)
	}
	if r.ContentPath != "" {
		field("path", r.ContentPath)
	}

	deadlines := "searching until " + r.SearchDeadline.Format("2006-01-02")
	if r.DownloadDeadline != nil {
		deadlines += ", this release until " + r.DownloadDeadline.Format("2006-01-02 15:04")
	}
	field("deadlines", deadlines)

	if r.LastError != "" {
		field("last error", r.LastError)
	}
	if r.StallCount > 0 {
		field("abandoned", fmt.Sprintf("%d release(s)", r.StallCount))
	}

	field("created", r.CreatedAt.Format("2006-01-02 15:04:05"))
	if r.CompletedAt != nil {
		field("finished", r.CompletedAt.Format("2006-01-02 15:04:05"))
	}
}

func writeEvents(out io.Writer, events []storage.EventRow) {
	if len(events) == 0 {
		fmt.Fprintln(out, "\nno events recorded")
		return
	}

	fmt.Fprintln(out, "\nhistory, newest first:")

	rows := [][]string{{"WHEN", "EVENT", "ACTOR", "DETAIL"}}
	for _, e := range events {
		rows = append(rows, []string{
			e.At.Format("2006-01-02 15:04:05"),
			e.Event,
			e.Actor,
			truncate(eventDetail(e.Detail), 60),
		})
	}

	for _, line := range padColumns(rows, 2) {
		fmt.Fprintln(out, "  "+line)
	}
}

// ---------------------------------------------------------------------------
// cancel
// ---------------------------------------------------------------------------

func cancelCmd(configFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <id|workflow-id>",
		Short: "Stop a request and clean up after it",
		Long: `Asks a request's workflow to stop.

If a torrent was already added, the workflow removes it and deletes its files
before finishing — so this is not immediate, and the request stays in its
current state until the cleanup has run. ` + "`mediad status`" + ` shows what it did.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Cancelling talks to Temporal, so it needs the write graph even
			// though it changes nothing in the database itself: the projection
			// is updated by the workflow as it unwinds.
			var deps writeDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				req, err := findRequest(ctx, deps.Store, args[0])
				if err != nil {
					return err
				}
				if !req.State.Active() {
					return fmt.Errorf("request %d is already %s", req.ID, req.State)
				}

				if err := deps.Req.Cancel(ctx, req.ID); err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Cancellation requested for request %d — %s\n",
					req.ID, requestTitle(req))
				fmt.Fprintf(out, "  it stays %s until the cleanup has run; "+
					"`mediad status %d` shows the outcome\n", req.State, req.ID)
				return nil
			})
		},
	}

	return cmd
}

// ---------------------------------------------------------------------------
// retry
// ---------------------------------------------------------------------------

func retryCmd(configFile *string) *cobra.Command {
	var (
		user   string
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "retry <id|workflow-id>",
		Short: "Start a stopped request searching again",
		Long: `Restarts a request that has finished, failed or been cancelled.

It is a fresh run of the same request, so its history and its watchers carry
over — and the search window starts again from now, because the two reasons to
retry are that the deadline passed and that too many releases were found
wanting. Releases already known to be bad stay blacklisted, so a retry looks
for something different rather than picking the same torrent again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var deps writeDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				req, err := findRequest(ctx, deps.Store, args[0])
				if err != nil {
					return err
				}

				actor := user
				if actor == "" {
					actor = defaultCLIUser()
				}

				restarted, err := deps.Req.Retry(ctx, request.RetryInput{
					RequestID:   req.ID,
					Actor:       "cli",
					RequestedBy: actor,
				})
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()

				if asJSON {
					return writeJSON(out, requestToJSON(restarted))
				}

				fmt.Fprintf(out, "Restarted request %d — %s\n", restarted.ID, requestTitle(restarted))
				fmt.Fprintf(out, "  searching now, until %s\n",
					restarted.SearchDeadline.Format("2006-01-02"))
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&user, "user", "", "who is asking, for the audit trail (default: $USER)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the request as JSON")

	return cmd
}

// ---------------------------------------------------------------------------
// notifications
// ---------------------------------------------------------------------------

func notificationsCmd(configFile *string) *cobra.Command {
	var (
		user   string
		unread bool
		limit  int
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "notifications",
		Short: "Show what happened to your requests",
		Long: `Shows the notifications recorded for a user.

Reading them here does not mark them read: the same rows are the web inbox and
the queue the Telegram bot delivers from, and a glance in a terminal should not
clear what somebody has not seen yet on their phone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var deps readDeps
			return withGraph(configFile, &deps, func(ctx context.Context) error {
				if user == "" {
					user = defaultCLIUser()
				}

				account, err := deps.Store.FindUserByCLI(ctx, user)
				if err != nil {
					if errors.Is(err, storage.ErrNotFound) {
						return fmt.Errorf("no CLI user called %q; "+
							"it is created the first time you request something", user)
					}
					return err
				}

				items, err := deps.Store.ListNotifications(ctx, account.ID, unread, limit)
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()

				if asJSON {
					rows := make([]notificationJSON, 0, len(items))
					for _, n := range items {
						rows = append(rows, notificationToJSON(n))
					}
					return writeJSON(out, rows)
				}

				if len(items) == 0 {
					fmt.Fprintf(out, "nothing for %s\n", user)
					return nil
				}

				for _, n := range items {
					fmt.Fprintf(out, "%s  %-9s %s\n",
						n.CreatedAt.Format("2006-01-02 15:04"), n.Kind, n.Title)
					if n.Body != "" {
						fmt.Fprintf(out, "                        %s\n", n.Body)
					}
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&user, "user", "", "whose notifications (default: $USER)")
	cmd.Flags().BoolVar(&unread, "unread", false, "only the ones not read yet")
	cmd.Flags().IntVar(&limit, "limit", 30, "how many to show")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// prompterFor returns a prompter, or nil when there is nobody to ask.
//
// The check is what keeps a command run from a script from printing a menu and
// then blocking on a stdin that will never answer. It is decided once, here,
// rather than by letting the read fail: the failing read produces
// "end of input", which reads like a bug in the tool rather than a flag that
// was not passed.
func prompterFor(cmd *cobra.Command) *prompt.Prompter {
	in := cmd.InOrStdin()

	f, ok := in.(*os.File)
	if !ok {
		return nil
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}

	return prompt.New(in, cmd.OutOrStdout())
}

// parseSeason reads the --season flag.
//
// The flag is a string, which is what lets "not given" be distinct from a
// number. A zero season is a real season — TMDB numbers specials as season 0 —
// so the zero value cannot mean "not given", and an integer flag would either
// need a sentinel like -1 printed in the help or would silently treat a
// mistyped season as a request for the specials.
func parseSeason(flag string) (*int, error) {
	if strings.TrimSpace(flag) == "" {
		return nil, nil
	}

	n, err := strconv.Atoi(flag)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("--season %q: want a season number, where 0 is the specials", flag)
	}
	return &n, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

// These types exist so that the JSON is a contract rather than a view of
// whatever the storage layer happens to hold. Encoding the storage struct
// directly would publish every column the moment one is added, and rename the
// output the moment one is.
type requestJSON struct {
	ID         int64  `json:"id"`
	WorkflowID string `json:"workflow_id"`
	State      string `json:"state"`

	MediaType string `json:"media_type"`
	TMDBID    int64  `json:"tmdb_id"`
	Season    *int   `json:"season,omitempty"`
	Title     string `json:"title"`
	TitleRU   string `json:"title_ru,omitempty"`
	Year      int    `json:"year,omitempty"`

	InPlex    bool   `json:"in_plex"`
	PlexTitle string `json:"plex_title,omitempty"`
	PlexYear  int    `json:"plex_year,omitempty"`

	Progress    float64 `json:"progress"`
	DownloadBps int64   `json:"download_bps,omitempty"`
	ETASeconds  int64   `json:"eta_seconds,omitempty"`

	Attempts         int        `json:"attempts"`
	NextRunAt        *time.Time `json:"next_run_at,omitempty"`
	SearchDeadline   time.Time  `json:"search_deadline"`
	DownloadDeadline *time.Time `json:"download_deadline,omitempty"`

	Selected          *selectedJSON `json:"selected,omitempty"`
	QbitHash          string        `json:"qbit_hash,omitempty"`
	ContentPath       string        `json:"content_path,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	AbandonedReleases int           `json:"abandoned_releases,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type selectedJSON struct {
	Title   string  `json:"title"`
	Indexer string  `json:"indexer,omitempty"`
	Size    int64   `json:"size_bytes,omitempty"`
	Seeders int     `json:"seeders"`
	Score   float64 `json:"score,omitempty"`
}

type eventJSON struct {
	At        time.Time       `json:"at"`
	Event     string          `json:"event"`
	FromState string          `json:"from_state,omitempty"`
	ToState   string          `json:"to_state,omitempty"`
	Actor     string          `json:"actor,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
}

type notificationJSON struct {
	ID        int64     `json:"id"`
	RequestID int64     `json:"request_id,omitempty"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func requestToJSON(r *storage.Request) requestJSON {
	out := requestJSON{
		ID:                r.ID,
		WorkflowID:        r.WorkflowID,
		State:             string(r.State),
		MediaType:         string(r.Media.Ref.Type),
		TMDBID:            r.Media.Ref.TMDBID,
		Season:            r.Media.Ref.Season,
		Title:             r.Media.Title,
		TitleRU:           r.Media.TitleRU,
		Year:              r.Media.Year,
		InPlex:            r.InPlex,
		PlexTitle:         r.PlexTitle,
		PlexYear:          r.PlexYear,
		Progress:          r.Progress,
		DownloadBps:       r.DlSpeed,
		ETASeconds:        r.ETASeconds,
		Attempts:          r.AttemptCount,
		NextRunAt:         r.NextRunAt,
		SearchDeadline:    r.SearchDeadline,
		DownloadDeadline:  r.DownloadDeadline,
		QbitHash:          r.QbitHash,
		ContentPath:       r.ContentPath,
		LastError:         r.LastError,
		AbandonedReleases: r.StallCount,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		CompletedAt:       r.CompletedAt,
	}

	if r.SelectedTitle != "" {
		out.Selected = &selectedJSON{
			Title:   r.SelectedTitle,
			Indexer: r.SelectedIndexer,
			Size:    r.SelectedSize,
			Seeders: r.SelectedSeeders,
			Score:   r.SelectedScore,
		}
	}
	return out
}

func eventToJSON(e storage.EventRow) eventJSON {
	return eventJSON{
		At:        e.At,
		Event:     e.Event,
		FromState: string(e.FromState),
		ToState:   string(e.ToState),
		Actor:     e.Actor,
		Detail:    e.Detail,
	}
}

func notificationToJSON(n storage.Notification) notificationJSON {
	return notificationJSON{
		ID:        n.ID,
		RequestID: n.RequestID,
		Kind:      string(n.Kind),
		Title:     n.Title,
		Body:      n.Body,
		CreatedAt: n.CreatedAt,
	}
}

// eventDetail renders an event's JSON detail on one line.
func eventDetail(detail json.RawMessage) string {
	if len(detail) == 0 {
		return ""
	}

	var generic map[string]any
	if err := json.Unmarshal(detail, &generic); err != nil {
		return string(detail)
	}

	parts := make([]string, 0, len(generic))
	for k, v := range generic {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
