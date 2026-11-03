package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

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
	ID    int64
	Title string
	// State is the machine-readable state, and the templates turn it into
	// a word with the vocabulary helpers. The code is kept rather than the
	// label because the code is what the filter and the stylesheet agree on.
	State     storage.State
	Detail    string
	Watchers  string
	When      string
	CreatedAt time.Time

	IsDownloading bool
	ProgressPct   string
	// ProgressValue is the same progress as a number, for the progress bar's
	// aria-valuenow. The percentage string is for the style attribute.
	ProgressValue int
	ProgressText  string
	SearchText    string

	// Age is how long the request has existed. It is the answer to the
	// question the product is actually about: a request that has been
	// looking for four months has to *look* like it, and not like a row
	// that is stuck.
	Age string
	// NextCheck is when the request will look again, or empty when it is
	// not waiting for a moment but for a person.
	NextCheck string
	// Attempts is how many times the request has searched.
	Attempts int
	// NeedsPick marks a request that is waiting for somebody to choose a
	// release. It is the one state that blocks on a person, and the row
	// says so with an action rather than with a word.
	NeedsPick bool

	PosterURL    string
	Year         int
	SelectedLink string

	// Href, CancelHref and RetryHref are the row's own links, with the
	// deployment's base path already in them. A row is a self-contained
	// thing: a template that has to reach for the page to build a URL
	// cannot be shared between pages, and the bucket tables are shared.
	Href       string
	CancelHref string
	RetryHref  string
}

// requestDetail is one request as a page.
type requestDetail struct {
	requestRow

	// Status is the one sentence that says what the bot is doing. It is the
	// highest-value line in the product: the difference between "the bot is
	// doing something" and "the bot has been doing something for four
	// months".
	Status string

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
	// SeasonText says how much of a season that is still being released is
	// on disk. It is empty for everything else.
	SeasonText string

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
// dashboardBucket is one time-scale of the dashboard.
//
// The buckets are split by how long a thing takes, not by which phase of the
// workflow it is in: a transfer takes seconds and a parked request takes two
// years, and they are different products wearing the same shirt. A request
// that has been searching for four months and a download at 90% do not belong
// in the same list.
type dashboardBucket struct {
	// Key names the bucket in the markup: the block's id, the count's id and
	// the table's id are all built from it.
	Key string
	// Title is the Russian name of the bucket.
	Title string
	// States are the request states that belong to it.
	States []storage.State
	// ShowProgress, ShowAge, ShowAttempts and ShowNext are the columns the
	// bucket carries besides the title and the state. A transfer has a
	// progress bar and nothing to wait for; a parked request has the other
	// three and no progress, because there is nothing moving.
	ShowProgress bool
	ShowAge      bool
	ShowAttempts bool
	ShowNext     bool
	// Empty is what the bucket says when it has nothing in it.
	Empty string
}

// The three buckets of the dashboard, in the order they are shown.
var dashboardBuckets = []dashboardBucket{
	{
		Key:   "downloading",
		Title: "Качается",
		States: []storage.State{
			storage.StateDownloading, storage.StateAdding,
		},
		ShowProgress: true,
		Empty:        "Ничего не качается.",
	},
	{
		Key:   "searching",
		Title: "Ищет",
		States: []storage.State{
			storage.StateSearching, storage.StatePending,
		},
		ShowAge:      true,
		ShowAttempts: true,
		ShowNext:     true,
		Empty:        "Ничего не ищется.",
	},
	{
		Key:   "waiting",
		Title: "Ждёт выбора",
		States: []storage.State{
			storage.StateManual, storage.StateOngoing,
		},
		ShowAge: true,
		Empty:   "Никто не ждёт выбора.",
	},
}

// bucketView is one bucket with the rows in it.
type bucketView struct {
	dashboardBucket
	Rows []requestRow
}

// buckets is the part of the dashboard that polls.
type buckets struct {
	List []bucketView
}

// dashboardView is the front page.
type dashboardView struct {
	page
	Filter dashboardFilter
	buckets
	Recent []requestRow
	// ActiveAll is the full set of active states, regardless of which subset
	// the user has filtered to. Used to render the state pills with their
	// counts.
	ActiveAll []storage.State
}

// cancelView is the confirmation page for a cancellation.
//
// The page exists because cancelling is not only a matter of stopping a
// search: the workflow takes the files of a download that has not been filed
// into the library yet off the disk. A button that says "Отменить" and
// nothing else hides that, and the person who finds out is the one who paid.
type cancelView struct {
	page
	Request requestDetail
	// Consequences are the lines that say what will happen, in the order a
	// person reads them.
	Consequences []string
}

// activeFragmentView is what the dashboard polls: the three buckets, and
// nothing else. The blocks themselves stay on the page — they are what the
// reader looks at, and replacing them every three seconds would throw away
// whether they are open.
type activeFragmentView struct {
	page
	Filter dashboardFilter
	buckets
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
	State string
	// States is every state the request can be in, for the filter. The
	// codes are what the query carries; the template asks the vocabulary
	// for the word.
	States   []storage.State
	Requests []requestRow
}

type requestView struct {
	page
	Request    requestDetail
	Candidates []storage.Candidate
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

// notificationOptions are the bits of the Telegram notify mask.
//
// One list for the settings page and for the form that saves it: the
// checkbox name is the option name, and a name that appears in one place and
// not the other is a setting the user cannot change.
func notificationOptions() []notificationOption {
	return []notificationOption{
		{Bit: 1, Name: "started", Label: "Поиск начался"},
		{Bit: 2, Name: "completed", Label: "Загрузка завершилась"},
		{Bit: 4, Name: "failed", Label: "Загрузка провалилась"},
		// Bit 8 covers two kinds on the watcher side: "skipped"
		// (the request was already in Plex) and "manual" (the workflow
		// is waiting for the user to pick a release from a candidate
		// list). They share the same bit because the user-facing
		// decision is the same: do you want the bot to bother you about
		// an out-of-band result?
		{Bit: 8, Name: "skipped_and_manual", Label: "Уже было в Plex или пора выбрать релиз"},
		// Bit 16 covers the season watcher: the request is waiting for
		// the rest of a season that is still being released, or the wait
		// ended without it.
		{Bit: 16, Name: "season", Label: "Сезон ещё выходит"},
	}
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

	Telegram    []telegramRow
	NewLinkCode string
	// BotUsername is the bot's @handle when known, empty otherwise. The
	// link-instruction text links to t.me/<BotUsername> when set, and
	// falls back to "the bot" when empty so the page is still useful on
	// a deployment where the Telegram API was unreachable at startup.
	BotUsername   string
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
//
// The base path comes in because the row carries its own links: the bucket
// tables are shared between the page and the polled fragment, and a template
// that has to reach for the page to build a URL cannot be shared.
func rowFor(r *storage.Request, watchers []storage.Watcher, now time.Time, basePath string) requestRow {
	row := requestRow{
		ID:        r.ID,
		Title:     titleOf(r),
		State:     r.State,
		CreatedAt: r.CreatedAt,
		When:      whenText(r, now),
		Age:       durationHuman(now.Sub(r.CreatedAt)),
		Attempts:  r.AttemptCount,
		// The one state that blocks on a person. The row says so with an
		// action, not with a word.
		NeedsPick: r.State == storage.StateManual,
		Href:      pathFunc(basePath, fmt.Sprintf("/requests/%d", r.ID)),
	}
	row.CancelHref = row.Href + "/cancel"
	row.RetryHref = row.Href + "/retry"

	// The next check is only meaningful for a request that is waiting for a
	// moment. One that is waiting for a person has no next check.
	if r.NextRunAt != nil {
		row.NextCheck = relativeTime(*r.NextRunAt)
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
	row.ProgressValue = progressValue(r)
	row.ProgressText = progressText(r)
	row.SearchText = searchText(r, now)
	row.PosterURL = pathFunc(basePath, posterPath(r.PosterPath, "w185"))
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
		// When the request will look again, not how long ago something
		// happened. The old helper rendered a future moment as "только что",
		// which said the opposite of the truth: a request that checks in six
		// hours was showing a last-run time.
		return relativeTime(*r.NextRunAt)
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
	case storage.StateOngoing:
		return seasonWaitText(r)
	case storage.StateCanceled:
		if r.CompletedAt != nil {
			return since(*r.CompletedAt)
		}
		return "отменён"
	default:
		return ""
	}
}

// seasonWaitText describes a request that is waiting for the rest of a
// season that is still being released.
//
// The counts are what makes the line useful: "ждёт остальные серии" says
// that something is pending, and "8 из 12" says how much of it is already
// on disk.
func seasonWaitText(r *storage.Request) string {
	if r.SelectedVideoCount > 0 && r.SelectedEpisodeCount > 0 {
		return fmt.Sprintf("ждёт остальные серии: %d из %d",
			r.SelectedVideoCount, r.SelectedEpisodeCount)
	}
	return "ждёт остальные серии"
}

// progressText describes a download in progress.
func progressText(r *storage.Request) string {
	if r.State != storage.StateDownloading {
		return ""
	}

	out := decimal(r.Progress*100) + "%"
	if r.DlSpeed > 0 {
		out += " · " + humanBytes(r.DlSpeed) + "/s"
	}
	if r.ETASeconds > 0 {
		out += " · осталось " + humanDuration(time.Duration(r.ETASeconds)*time.Second)
	}
	return out
}

// progressValue is the download progress as a whole percentage.
//
// It is a number rather than the string the style attribute wants, because
// the progress bar's aria-valuenow has to be a number: a screen reader
// announces "45" and not "45%".
func progressValue(r *storage.Request) int {
	if r.State != storage.StateDownloading {
		return 0
	}
	v := int(r.Progress * 100)
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
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
			next = relativeTime(*r.NextRunAt)
		}
		return fmt.Sprintf("попыток: %d, следующая проверка %s", r.AttemptCount, next)
	default:
		return fmt.Sprintf("попыток: %d", r.AttemptCount)
	}
}

// detailFor renders one request as a page.
//
// The candidate count is a parameter rather than something it looks up: the
// handler already has the list, and the count is what the status line says
// about a request waiting for a person.
func detailFor(r *storage.Request, watchers []storage.Watcher, events []storage.EventRow, candidates int, now time.Time, basePath string) requestDetail {
	d := requestDetail{
		requestRow:  rowFor(r, watchers, now, basePath),
		Status:      statusLine(r, now, candidates),
		WorkflowID:  r.WorkflowID,
		QbitHash:    r.QbitHash,
		ContentPath: r.ContentPath,
		LastError:   r.LastError,
		Abandoned:   r.StallCount,
		Active:      r.State.Active(),
		PlexText:    plexText(r),
	}

	// The season line only exists while the request is waiting for the rest
	// of a season that is still being released.
	if r.State == storage.StateOngoing {
		d.SeasonText = seasonWaitText(r)
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

// statusLine is the one sentence that says what the bot is doing.
//
// It is the highest-value line in the product: the difference between "the
// bot is doing something" and "the bot has been doing something for four
// months". Every number in it comes from the request itself, because a
// sentence that could be wrong is worse than no sentence at all.
func statusLine(r *storage.Request, now time.Time, candidates int) string {
	return capitalise(statusSentence(r, now, candidates))
}

// capitalise puts the first letter of a sentence in upper case.
//
// The sentences are built from phrases that were written to stand inside a
// sentence — "ждёт остальные серии: 5 из 8" — and the line is a sentence of
// its own.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}

// statusSentence is the body of the status line.
func statusSentence(r *storage.Request, now time.Time, candidates int) string {
	switch r.State {
	case storage.StateSearching:
		out := "Ищет релиз с русским звуком."
		if r.AttemptCount > 0 {
			out += " Проверено " + nounTime.count(r.AttemptCount) + " за " +
				durationHuman(now.Sub(r.CreatedAt)) + "."
		}
		if r.NextRunAt != nil {
			out += " Следующая проверка " + relativeTime(*r.NextRunAt) + "."
		}
		return out

	case storage.StatePending:
		if r.AttemptCount == 0 {
			return "Ждёт первой проверки."
		}
		out := "Проверено " + nounTime.count(r.AttemptCount) + "."
		if r.NextRunAt != nil {
			out += " Следующая проверка " + relativeTime(*r.NextRunAt) + "."
		}
		return out

	case storage.StateDownloading:
		pct := progressValue(r)
		if r.SelectedSize > 0 {
			done := int64(float64(r.SelectedSize) * r.Progress)
			return fmt.Sprintf("Качает %s из %s (%d%%).",
				humanBytes(done), humanBytes(r.SelectedSize), pct)
		}
		return fmt.Sprintf("Качает, %d%%.", pct)

	case storage.StateAdding:
		return "Добавляет релиз в клиент."

	case storage.StateManual:
		if candidates == 0 {
			return "Индексатор ничего не вернул. Релиз можно прикрепить вручную."
		}
		return fmt.Sprintf("Индексатор вернул %s, авто-подбор не выбрал ни один. Нужен ваш выбор.",
			nounCandidate.count(candidates))

	case storage.StateOngoing:
		return seasonWaitText(r) + ". Сезон ещё выходит."

	case storage.StateCompleted:
		return "Загрузка завершена."

	case storage.StateFailed:
		if r.ErrorStreak > 0 {
			return "Поиск остановлен после " + nounError.countG(r.ErrorStreak) + " подряд."
		}
		return "Поиск остановлен."

	case storage.StateCanceled:
		return "Запрос отменён."

	case storage.StateSkipped:
		return "Уже есть в Plex."

	default:
		return ""
	}
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

// cancelConsequences says what a cancellation will do to this request.
//
// The rules come from the workflow, not from this page. A cancelled download
// has its files deleted — it was never filed into the library, so nothing on
// the disk is somebody's. The exception is a request waiting for the rest of
// a season: its pack is finished and already in Plex, and deleting those
// files would take off the disk the episodes somebody is watching.
//
// The state is read at render time, which is as close to the truth as a
// confirmation page can get: the request may move on between the page being
// rendered and the button being pressed.
func cancelConsequences(r *storage.Request) []string {
	out := []string{"Поиск остановится: запрос больше не будет проверяться."}

	switch {
	case r.State == storage.StateOngoing:
		out = append(out,
			"Торрент будет удалён из клиента, но уже скачанные серии останутся на диске — "+
				"они уже в Plex.")
	case r.QbitHash != "":
		out = append(out,
			"Торрент и уже скачанные файлы будут удалены с диска.")
	default:
		out = append(out,
			"На диске ничего нет: загрузка ещё не началась.")
	}

	out = append(out,
		"Сам запрос останется в истории, и его можно будет запустить снова.")
	return out
}

// cancelOutcome is the one-line answer after a cancellation.
//
// The same rule as the confirmation, in one sentence: what happened to the
// files is the part somebody will want to know an hour later.
func cancelOutcome(r *storage.Request) string {
	switch {
	case r.State == storage.StateOngoing:
		return "Запрос отменён. Торрент удалён, скачанные серии остались на диске."
	case r.QbitHash != "":
		return "Запрос отменён. Торрент и скачанные файлы удалены."
	default:
		return "Запрос отменён."
	}
}

// ---------------------------------------------------------------------------
// Small renderers
// ---------------------------------------------------------------------------

// decimal renders a number the way a Russian reader writes it.
//
// The dot is the machine's separator; the comma is the reader's. A size
// that says "21.0 ГиБ" in a Russian interface reads as a machine talking.
func decimal(v float64) string {
	return strings.Replace(fmt.Sprintf("%.1f", v), ".", ",", 1)
}

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
			return decimal(value) + " " + u
		}
	}
	return decimal(value) + " ПиБ"
}

// humanDuration renders an interval at the resolution somebody cares about.
//
// A single unit is spelled out, because Russian pluralises it and an
// abbreviation cannot: "1 минуту" and "5 минут" are different words. Two
// units keep the count in front of each, so the sentence still reads as a
// countdown rather than as a list.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return nounSecond.countA(int(d.Seconds()))
	case d < time.Hour:
		return nounMinute.countA(int(d.Minutes()))
	case d < 48*time.Hour:
		return nounHour.count(int(d.Hours())) + " " + nounMinute.countA(int(d.Minutes())%60)
	default:
		return nounDay.count(int(d.Hours() / 24))
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
