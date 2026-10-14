package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// The view models. They exist so that a template cannot reach into a storage
// struct and start depending on a column: everything a page shows is named
// here, and adding a column to the database does not change any page until it
// is added below on purpose.

// requestRow is one request as a table row.
type requestRow struct {
	ID        int64
	Title     string
	State     string
	Detail    string
	Watchers  string
	When      string
	CreatedAt time.Time

	IsDownloading bool
	ProgressPct   string
	ProgressText  string
	SearchText    string

	PosterURL    string
	Year         int
	SelectedLink string
}

// requestDetail is one request as a page.
type requestDetail struct {
	requestRow

	WorkflowID  string
	QbitHash    string
	ContentPath string
	Deadlines   string
	LastError   string
	Abandoned   int
	PlexText    string
	ReleaseText string
	Created     string
	CompletedAt string
	Active      bool

	Events []eventRow
}

type eventRow struct {
	At     string
	Event  string
	Actor  string
	Detail string
}

// searchCard is one TMDB hit.
type searchCard struct {
	Type      string
	TMDBID    int64
	Title     string
	Original  string
	Year      int
	Kind      string
	PosterURL string
	// Rating is TMDB's user score (0–10), zero when the title has no votes.
	// Not IMDb: TMDB does not expose the IMDb score, this is the closest
	// available signal without a second API key.
	Rating float64
}

// seasonCard is one season of a series.
type seasonCard struct {
	Number   int
	Episodes int
	AirDate  string
	Upcoming bool
}

// confirmation is the last page before a request is created.
type confirmation struct {
	MediaType string
	TMDBID    int64
	Title     string
	Kind      string
	Year      int
	Runtime   int
	Overview  string
	PosterURL string
	HasSeason bool
	Season    int
}

// The per-page view models.
//
// Named rather than anonymous, and that is for a concrete reason: a handler
// that builds an anonymous struct inline cannot be rendered by a test without
// restating its shape, so the template ends up checked by nothing. These are
// also the contract with the template — a field removed here is a compile
// error in the handler that sets it, rather than a blank cell.
type dashboardView struct {
	page
	Filter dashboardFilter
	Active []requestRow
	Recent []requestRow
	// ActiveAll is the full set of active states, regardless of which subset
	// the user has filtered to. Used to render the state pills with their
	// counts.
	ActiveAll []storage.State
}

type activeFragmentView struct {
	page
	Filter    dashboardFilter
	Active    []requestRow
	ActiveAll []storage.State
}

type searchView struct {
	page
	Query   string
	Results []searchCard
}

type seasonsView struct {
	page
	Show    showHeader
	Seasons []seasonCard
}

type showHeader struct {
	TMDBID    int64
	Title     string
	Year      int
	PosterURL string
}

type confirmView struct {
	page
	confirmation
}

type requestsView struct {
	page
	State    string
	States   []string
	Requests []requestRow
}

type requestView struct {
	page
	Request requestDetail
}

type notificationRow struct {
	ID        int64
	RequestID int64
	Title     string
	Body      string
	When      string
	Unread    bool
	PosterURL string
}

type notificationsView struct {
	page
	Items []notificationRow
}

type inviteRow struct {
	Code      string
	CreatedAt string
	ExpiresAt string
	State     string
}

// telegramRow is one chat attached to the current user.
type telegramRow struct {
	ID        string
	CreatedAt string
}

// notificationOption is one bit of the Telegram notify mask, rendered as a
// checkbox. The bit and the label are split because a missing key is what
// the user is signalling by leaving the box empty.
type notificationOption struct {
	Bit     int
	Label   string
	Name    string
	Checked bool
}

type settingsView struct {
	page
	Tab string

	Invites   []inviteRow
	Users     []storage.User
	InviteTTL string

	// NewInvite, when set, is a freshly issued invite code whose full
	// /join?code=… URL the page should render as a clickable link. The
	// code comes from ?new_invite=… on the redirect after a successful
	// POST to /settings/invites; it is one-shot, refreshed only when the
	// admin issues another.
	NewInvite string
	// NewInviteURL is the absolute path the admin can hand to the
	// invitee, including the base path. Rendered as a copyable link.
	NewInviteURL string

	Telegram      []telegramRow
	NewLinkCode   string
	NotifyOptions []notificationOption
}

type loginView struct {
	page
	Next string
}

// helpView is the static help page. No data beyond what the layout already
// carries: the page is documentation, not a query.
type helpView struct {
	page
}

// ---------------------------------------------------------------------------
// Building rows
// ---------------------------------------------------------------------------

// rowFor renders one request as a table row.
func rowFor(r *storage.Request, watchers []storage.Watcher, now time.Time) requestRow {
	row := requestRow{
		ID:        r.ID,
		Title:     titleOf(r),
		State:     string(r.State),
		CreatedAt: r.CreatedAt,
		When:      whenText(r, now),
	}

	if len(watchers) > 0 {
		names := make([]string, 0, len(watchers))
		for _, w := range watchers {
			if w.Username != "" {
				names = append(names, w.Username)
			}
		}
		row.Watchers = strings.Join(names, ", ")
	}

	row.IsDownloading = r.State == storage.StateDownloading
	row.ProgressPct = percent(r.Progress)
	row.ProgressText = progressText(r)
	row.SearchText = searchText(r, now)
	row.PosterURL = posterPath(r.PosterPath, "w185")
	row.Year = r.Media.Year
	row.SelectedLink = r.SelectedLink

	return row
}

// titleOf names a request the way a person does.
func titleOf(r *storage.Request) string {
	title := r.Media.DisplayTitle()
	if r.Media.Year > 0 {
		title += fmt.Sprintf(" (%d)", r.Media.Year)
	}
	if r.Media.Ref.IsSeason() {
		title += fmt.Sprintf(" · сезон %d", r.Media.Ref.SeasonNumber())
	}
	return title
}

// whenText is the column that changes per state.
func whenText(r *storage.Request, now time.Time) string {
	switch r.State {
	case storage.StateDownloading:
		return progressText(r)
	case storage.StatePending, storage.StateSearching:
		if r.NextRunAt == nil || !r.NextRunAt.After(now) {
			return "вот-вот"
		}
		return since(*r.NextRunAt) // negative durations render as "только что"
	case storage.StateSkipped:
		if r.PlexTitle != "" {
			return "уже в Plex: " + r.PlexTitle
		}
		return "уже в Plex"
	case storage.StateFailed:
		if r.LastError != "" {
			return r.LastError
		}
		return "не удалось"
	case storage.StateCompleted:
		if r.CompletedAt != nil {
			return since(*r.CompletedAt)
		}
		return "готово"
	case storage.StateCanceled:
		if r.CompletedAt != nil {
			return since(*r.CompletedAt)
		}
		return "отменён"
	default:
		return ""
	}
}

// progressText describes a download in progress.
func progressText(r *storage.Request) string {
	if r.State != storage.StateDownloading {
		return ""
	}

	out := fmt.Sprintf("%.1f%%", r.Progress*100)
	if r.DlSpeed > 0 {
		out += " · " + humanBytes(r.DlSpeed) + "/s"
	}
	if r.ETASeconds > 0 {
		out += " · осталось " + humanDuration(time.Duration(r.ETASeconds)*time.Second)
	}
	return out
}

// searchText describes where a request is in the search loop.
func searchText(r *storage.Request, now time.Time) string {
	switch {
	case r.State == storage.StateSearching:
		return fmt.Sprintf("ищет сейчас, попыток: %d", r.AttemptCount)
	case r.State == storage.StatePending && r.AttemptCount == 0:
		return "ждёт первого поиска"
	case r.State == storage.StatePending:
		next := "вот-вот"
		if r.NextRunAt != nil && r.NextRunAt.After(now) {
			next = "через " + humanDuration(r.NextRunAt.Sub(now))
		}
		return fmt.Sprintf("попыток: %d, следующая %s", r.AttemptCount, next)
	default:
		return fmt.Sprintf("попыток: %d", r.AttemptCount)
	}
}

// detailFor renders one request as a page.
func detailFor(r *storage.Request, watchers []storage.Watcher, events []storage.EventRow, now time.Time) requestDetail {
	d := requestDetail{
		requestRow:  rowFor(r, watchers, now),
		WorkflowID:  r.WorkflowID,
		QbitHash:    r.QbitHash,
		ContentPath: r.ContentPath,
		LastError:   r.LastError,
		Abandoned:   r.StallCount,
		Active:      r.State.Active(),
		PlexText:    plexText(r),
	}

	// Formatted here rather than in the template: a time.Time printed directly
	// carries its monotonic clock reading and zone name, which is how a page
	// ends up saying "01:14:00.91413 +0300 MSK".
	d.Created = r.CreatedAt.Format("2006-01-02 15:04")
	if r.CompletedAt != nil {
		d.CompletedAt = r.CompletedAt.Format("2006-01-02 15:04")
	}

	if r.SelectedTitle != "" {
		parts := []string{r.SelectedTitle}
		if r.SelectedIndexer != "" {
			parts = append(parts, r.SelectedIndexer)
		}
		if r.SelectedSize > 0 {
			parts = append(parts, humanBytes(r.SelectedSize))
		}
		parts = append(parts, fmt.Sprintf("сидов: %d", r.SelectedSeeders))
		d.ReleaseText = strings.Join(parts, " · ")
	}

	deadline := "ищем до " + r.SearchDeadline.Format("2006-01-02")
	if r.DownloadDeadline != nil {
		deadline += ", этот релиз до " + r.DownloadDeadline.Format("2006-01-02 15:04")
	}
	d.Deadlines = deadline

	for _, e := range events {
		d.Events = append(d.Events, eventRow{
			At:     e.At.Format("2006-01-02 15:04:05"),
			Event:  e.Event,
			Actor:  e.Actor,
			Detail: eventDetail(e.Detail),
		})
	}

	return d
}

// plexText says what the library check found.
//
// What matched is shown, not just whether something did: a skip is the one
// outcome a user cannot verify themselves, and a wrong match that says so is
// fixable where a bare "already there" is not.
func plexText(r *storage.Request) string {
	switch {
	case r.InPlex && r.PlexTitle != "":
		out := "есть: " + r.PlexTitle
		if r.PlexYear > 0 {
			out += fmt.Sprintf(" (%d)", r.PlexYear)
		}
		return out
	case r.InPlex:
		return "есть в библиотеке"
	default:
		return "нет в библиотеке"
	}
}

// ---------------------------------------------------------------------------
// Small renderers
// ---------------------------------------------------------------------------

// humanBytes renders a size the way a person reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}

	value := float64(n)
	for _, u := range []string{"КиБ", "МиБ", "ГиБ", "ТиБ"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f ПиБ", value)
}

// humanDuration renders an interval at the resolution somebody cares about.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч %d мин", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d дн", int(d.Hours()/24))
	}
}

// searchCardFor renders one TMDB result.
func searchCardFor(r tmdb.SearchResult) searchCard {
	kind := "фильм"
	if r.Ref.Type == domain.MediaTypeTV {
		kind = "сериал"
	}

	card := searchCard{
		Type:   string(r.Ref.Type),
		TMDBID: r.Ref.TMDBID,
		Title:  r.Title,
		Year:   r.Year,
		Kind:   kind,
		// Our own endpoint, not tmdb.PosterURL: the browser may not be able to
		// reach image.tmdb.org at all, which is the whole reason this exists.
		PosterURL: posterPath(r.PosterPath, defaultPosterSize),
		Rating:    r.VoteAverage,
	}
	if r.OriginalTitle != r.Title {
		card.Original = r.OriginalTitle
	}
	return card
}

// confirmationFor renders the page that precedes a request.
func confirmationFor(spec domain.MediaSpec, poster string) confirmation {
	kind := "Фильм"
	if spec.Ref.Type == domain.MediaTypeTV {
		kind = "Сериал"
	}

	if poster == "" {
		poster = spec.PosterPath
	}

	c := confirmation{
		MediaType: string(spec.Ref.Type),
		TMDBID:    spec.Ref.TMDBID,
		Title:     spec.DisplayTitle(),
		Kind:      kind,
		Year:      spec.Year,
		Runtime:   spec.RuntimeMin,
		Overview:  spec.Overview,
		// The larger size, since this page shows one poster rather than a grid.
		PosterURL: posterPath(poster, "w500"),
	}
	if spec.Ref.IsSeason() {
		c.HasSeason = true
		c.Season = spec.Ref.SeasonNumber()
	}
	return c
}

// eventDetail flattens an event's JSON detail onto one line.
func eventDetail(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return truncate(string(raw), 80)
	}
	if len(generic) == 0 {
		return ""
	}

	keys := make([]string, 0, len(generic))
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, generic[k]))
	}
	return truncate(strings.Join(parts, " "), 100)
}

// truncate shortens a string for a table cell, on rune boundaries.
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
