package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/fx"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/temporalx"
	"github.com/alex/mediad/internal/tmdb"
)

// The command-line interface talks to Postgres and Temporal directly, so it
// works whether or not the api process is running, and it is what makes the
// project usable before either of the other two interfaces exists.

const (
	// cliTimeout bounds a command once the graph is up.
	//
	// It is generous because some of these commands ask a person a question,
	// and a timeout that expires while somebody is deciding which of five
	// films they meant is worse than no timeout at all. It is bounded at all
	// only so that a hung database or indexer does not leave a terminal
	// waiting forever.
	cliTimeout = 5 * time.Minute
)

// ---------------------------------------------------------------------------
// Dependency graph
// ---------------------------------------------------------------------------

// readDeps is what a command that only reads from the database asks for.
//
// The fields are per command rather than shared, and that is the point: fx
// constructs only what is asked for, so a command that does not name the
// Temporal client never dials Temporal. Naming it here "just in case" would
// make `mediad list` refuse to run whenever the worker is down — which is
// exactly when somebody reaches for it to find out what is going on.
type readDeps struct {
	fx.In
	Store *storage.Store
}

// writeDeps is what a command that starts or stops workflows asks for.
//
// Naming *request.Service is what pulls in the Temporal client and the
// connectivity check; the configuration is not listed because nothing here
// reads it directly, and a field kept "in case" would make every command wait
// on dependencies it does not use.
type writeDeps struct {
	fx.In
	Store *storage.Store
	TMDB  *tmdb.Client
	Req   *request.Service
}

// withGraph builds the graph a one-shot command needs, runs fn against it, and
// tears it down.
//
// The two contexts are deliberately different. Starting the graph is bounded
// tightly, because connecting to Postgres and applying migrations is not
// something a person should wait on indefinitely. Running the command is not,
// because it may be waiting on a person.
func withGraph(configFile *string, deps any, fn func(ctx context.Context) error) error {
	app := fx.New(
		graph(configFile),
		// Verification only where a workflow is involved: a read-only command
		// has no business requiring the workflow engine to be reachable.
		fx.Options(conditionalDeps(deps)),
		fx.Populate(deps),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		return err
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), cliTimeout)
	defer cancelRun()

	runErr := fn(runCtx)

	stopCtx, cancelStop := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStop()

	if err := app.Stop(stopCtx); err != nil && runErr == nil {
		return err
	}
	return runErr
}

// conditionalDeps adds the Temporal connectivity check for the commands that
// use the workflow engine.
//
// The namespace check is worth having here for one reason: a namespace that
// does not exist fails later, on the first workflow start, with a message that
// reads like an application bug rather than a setup step that was skipped.
func conditionalDeps(deps any) fx.Option {
	switch deps.(type) {
	case *writeDeps:
		return temporalx.ConnectivityModule
	default:
		return fx.Options()
	}
}

// ---------------------------------------------------------------------------
// Identifying a request
// ---------------------------------------------------------------------------

// findRequest resolves an argument that may be a numeric id or a workflow id.
//
// Both are accepted because both are in front of the user: `list` prints the
// number, and the Temporal UI — where an operator goes when something is
// wedged — shows "movie-603". Requiring a translation between them would be
// busywork at the worst possible moment.
func findRequest(ctx context.Context, store *storage.Store, arg string) (*storage.Request, error) {
	if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
		return store.GetRequest(ctx, id)
	}
	return store.FindRequestByWorkflowID(ctx, arg)
}

// ---------------------------------------------------------------------------
// Choosing a user
// ---------------------------------------------------------------------------

// defaultCLIUser is the name a command acts as when none is given.
//
// The operating-system user name, so that running the CLI as yourself
// attributes the request to you and the notifications land in your inbox.
func defaultCLIUser() string {
	for _, key := range []string{"MEDIAD_USER", "USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "cli"
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// requestTitle is the media as a person names it: the Russian title where
// there is one, the year, and the season for a series.
func requestTitle(r *storage.Request) string {
	title := r.Media.DisplayTitle()
	if r.Media.Year > 0 {
		title += fmt.Sprintf(" (%d)", r.Media.Year)
	}
	if r.Media.Ref.IsSeason() {
		title += fmt.Sprintf(", сезон %d", r.Media.Ref.SeasonNumber())
	}
	return title
}

// requestWhen renders what a request is waiting for, or how it ended.
//
// Each state has a different useful answer, and one column with all of them is
// better than five that are empty in turn.
func requestWhen(r *storage.Request, now time.Time) string {
	switch r.State {
	case storage.StateDownloading:
		out := percent(r.Progress)
		if r.DlSpeed > 0 {
			out += " at " + bytesPerSecond(r.DlSpeed)
		}
		if r.ETASeconds > 0 {
			out += ", " + shortDuration(time.Duration(r.ETASeconds)*time.Second) + " left"
		}
		return out

	case storage.StatePending, storage.StateSearching:
		if r.NextRunAt == nil {
			return "due now"
		}
		if !r.NextRunAt.After(now) {
			return "due now"
		}
		return "next in " + shortDuration(r.NextRunAt.Sub(now))

	case storage.StateSkipped:
		if r.PlexTitle != "" {
			out := "in Plex: " + r.PlexTitle
			if r.PlexYear > 0 {
				out += fmt.Sprintf(" (%d)", r.PlexYear)
			}
			return out
		}
		return "already in the library"

	case storage.StateCompleted:
		if r.CompletedAt != nil {
			return "finished " + r.CompletedAt.Format("2006-01-02 15:04")
		}
		return "finished"

	case storage.StateFailed:
		if r.LastError != "" {
			return truncate(r.LastError, 60)
		}
		return "failed"

	case storage.StateCanceled:
		if r.CompletedAt != nil {
			return "canceled " + r.CompletedAt.Format("2006-01-02 15:04")
		}
		return "canceled"

	default:
		return ""
	}
}

// describeSearch renders where a request is in the search loop.
func describeSearch(r *storage.Request, now time.Time) string {
	switch {
	case r.State == storage.StateSearching:
		return fmt.Sprintf("searching now, %d so far", r.AttemptCount)
	case r.State == storage.StatePending && r.AttemptCount == 0:
		return "waiting for the first search"
	case r.State == storage.StatePending:
		next := "due now"
		if r.NextRunAt != nil && r.NextRunAt.After(now) {
			next = "next " + r.NextRunAt.Format("2006-01-02 15:04")
		}
		return fmt.Sprintf("%d so far, %s", r.AttemptCount, next)
	default:
		// "attempts", not "searches": the label above this already says
		// search, and "search 47 searches" is what happens when the two are
		// written independently.
		return fmt.Sprintf("%d attempts", r.AttemptCount)
	}
}

// describeSelected renders the release being downloaded, if there is one.
func describeSelected(r *storage.Request) string {
	if r.SelectedTitle == "" {
		return ""
	}

	out := r.SelectedTitle
	var facts []string
	if r.SelectedIndexer != "" {
		facts = append(facts, r.SelectedIndexer)
	}
	if r.SelectedSize > 0 {
		facts = append(facts, humanBytes(r.SelectedSize))
	}
	facts = append(facts, fmt.Sprintf("%d seeders", r.SelectedSeeders))
	if r.SelectedScore > 0 {
		facts = append(facts, fmt.Sprintf("score %.2f", r.SelectedScore))
	}
	return out + "\n                  " + strings.Join(facts, ", ")
}

// describePlex renders the library status, which is what a skip is decided on.
func describePlex(r *storage.Request) string {
	switch {
	case r.InPlex && r.PlexTitle != "":
		out := "in the library as " + r.PlexTitle
		if r.PlexYear > 0 {
			out += fmt.Sprintf(" (%d)", r.PlexYear)
		}
		return out
	case r.InPlex:
		return "in the library"
	default:
		return "not in the library"
	}
}

func percent(f float64) string {
	return fmt.Sprintf("%.1f%%", f*100)
}

// humanBytes renders a size the way a person reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f EiB", value)
}

// bytesPerSecond renders a transfer rate.
func bytesPerSecond(n int64) string {
	if n <= 0 {
		return "0 B/s"
	}
	return humanBytes(n) + "/s"
}

// shortDuration renders an interval at a resolution a person cares about.
//
// The point is to be readable in a table, not precise: "3h 12m" is what
// somebody wants from a column that answers "when".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return strings.TrimSpace(fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncate shortens a string for a table cell.
//
// It cuts on runes rather than bytes: Russian titles and error messages are the
// common case here, and cutting mid-character produces a replacement glyph
// where a clean ellipsis belongs.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// padColumns pads each cell to the widest in its column.
//
// Written by hand rather than with text/tabwriter because the widths have to
// account for the terminal's own idea of how wide a Cyrillic character is, and
// tabwriter counts runes. It is close enough in practice; this only has to
// look tidy.
func padColumns(rows [][]string, gap int) []string {
	if len(rows) == 0 {
		return nil
	}

	widths := make([]int, 0, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				widths = append(widths, 0)
			}
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				// Never pad the last cell: trailing spaces are invisible in a
				// terminal and travel badly through a pipe.
				b.WriteString(cell)
				break
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+gap))
		}
		out = append(out, b.String())
	}
	return out
}

// displayWidth approximates how many terminal columns a string occupies.
//
// Cyrillic counts as one, which is worth stating because the opposite is a
// natural guess: Unicode files it under East Asian Width "Ambiguous", and
// terminals resolve that to a single cell outside a CJK locale. So the Russian
// titles this project prints most of are measured exactly, and the ranges
// below are the genuinely double-width ones, which a library holding a
// Japanese or Chinese film will contain.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
			r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi, CJK symbols
			r >= 0x3041 && r <= 0x33FF, // kana through CJK compatibility
			r >= 0x3400 && r <= 0x4DBF, // CJK extension A
			r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
			r >= 0xA000 && r <= 0xA4CF, // Yi
			r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
			r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
			r >= 0xFE30 && r <= 0xFE6F, // CJK compatibility forms
			r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
			r >= 0xFFE0 && r <= 0xFFE6:
			w += 2
		default:
			w++
		}
	}
	return w
}

// sortedStates renders a state filter for an error message.
func sortedStates(states []storage.State) string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ensureMediaType parses a media type argument.
func ensureMediaType(s string) (domain.MediaType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "movie", "film", "movies", "фильм":
		return domain.MediaTypeMovie, nil
	case "tv", "series", "show", "сериал":
		return domain.MediaTypeTV, nil
	default:
		return "", fmt.Errorf("unknown media type %q: want movie or tv", s)
	}
}
