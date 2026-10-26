package web

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
	"github.com/alex/mediad/internal/torrentfile"
)

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

// dashboardLimit caps the lists on the front page.
const dashboardLimit = 50

// dashboardFilter is the parsed query string for the dashboard. It carries
// both the values used to query storage and the same values formatted back for
// the template's form: that way a filter that round-tripped through the URL
// renders the controls in the same state the user left them.
type dashboardFilter struct {
	Q          string   // free-text search across title fields
	Type       string   // "movie" / "tv" / "" — empty means both
	Sort       string   // "date" (default) / "title"
	Since      string   // "24h" / "7d" / "30d" / "" (none = no limit)
	ActiveSubs []string // subset of ActiveStates the user wants shown; empty = all
}

// toStorage turns the parsed values into a storage.RequestFilter. Since and
// SortByTitle come from the same source, so they live together here. The
// dashboard's Active list always asks for all active states, and then trims
// in the template; the per-state filter only narrows what the user sees.
func (f dashboardFilter) toStorage(activeDefaultLimit int, recentLimit int) (active storage.RequestFilter, recent storage.RequestFilter) {
	common := func(states []storage.State, limit int) storage.RequestFilter {
		return storage.RequestFilter{
			States:      states,
			MediaType:   domain.MediaType(f.Type),
			Query:       f.Q,
			SortByTitle: f.Sort == "title",
			Since:       sinceDuration(f.Since),
			Limit:       limit,
		}
	}
	active = common(storage.ActiveStates(), activeDefaultLimit)
	active.States = applyActiveSubset(active.States, f.ActiveSubs)

	recentStates := []storage.State{
		storage.StateCompleted, storage.StateFailed,
		storage.StateCanceled, storage.StateSkipped,
	}
	recent = common(recentStates, recentLimit)
	return
}

// activeStatesForTemplate reports which of the four active states are
// currently visible to the user. Used to render the pills.
func (f dashboardFilter) activeStatesForTemplate() []storage.State {
	all := storage.ActiveStates()
	return applyActiveSubset(all, f.ActiveSubs)
}

// MirrorStates reports the state pills that are checked.
//
// The dashboard's two filter forms each mirror the other's controls, and
// the pills live in the Active block's form. This is what the shared
// filters' form has to carry to keep the selection across a submission.
//
// An empty subset means "no filter" — every state is visible and every
// pill is checked — so the mirror is every active state.
func (f dashboardFilter) MirrorStates() []storage.State {
	if len(f.ActiveSubs) == 0 {
		return storage.ActiveStates()
	}
	out := make([]storage.State, 0, len(f.ActiveSubs))
	for _, s := range f.ActiveSubs {
		out = append(out, storage.State(s))
	}
	return out
}

// ActiveFragmentQS is the "?q=...&state=...&type=...&sort=..." query the
// dashboard builds into the htmx poll URL so the fragment follows the same
// filter as the page it sits inside. Since is intentionally dropped here:
// it is a window for the Recent block, not the Active one.
func (f dashboardFilter) ActiveFragmentQS() string {
	parts := make([]string, 0, 3+len(f.ActiveSubs))
	if f.Q != "" {
		parts = append(parts, "q="+url.QueryEscape(f.Q))
	}
	for _, s := range f.ActiveSubs {
		parts = append(parts, "state="+url.QueryEscape(s))
	}
	if f.Type != "" {
		parts = append(parts, "type="+url.QueryEscape(f.Type))
	}
	if f.Sort != "" {
		parts = append(parts, "sort="+url.QueryEscape(f.Sort))
	}
	return "?" + strings.Join(parts, "&")
}

// applyActiveSubset keeps only the states the user wants. If the user
// disabled none, the default (all) is returned unchanged.
func applyActiveSubset(all []storage.State, subs []string) []storage.State {
	if len(subs) == 0 {
		return all
	}
	wanted := make(map[storage.State]bool, len(subs))
	for _, s := range subs {
		wanted[storage.State(s)] = true
	}
	out := make([]storage.State, 0, len(all))
	for _, s := range all {
		if wanted[s] {
			out = append(out, s)
		}
	}
	return out
}

// sinceDuration maps the since= string to a time.Duration. Unknown values
// return zero (no lower bound).
func sinceDuration(s string) time.Duration {
	switch s {
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default:
		return 0
	}
}

// parseDashboardFilter reads the URL query and returns a dashboardFilter.
// Unknown values fall back to defaults; an empty result means "no filter".
func parseDashboardFilter(r *http.Request) dashboardFilter {
	q := r.URL.Query()
	f := dashboardFilter{
		Q:     strings.TrimSpace(q.Get("q")),
		Type:  mediaTypeFilter(q.Get("type")),
		Sort:  sortFilter(q.Get("sort")),
		Since: sinceFilter(q.Get("since")),
	}
	// Multiple "state" params come from a series of <input type="checkbox"
	// name="state">, one per active state. An empty list means "no subset
	// chosen", which we treat as the default — every state visible — so the
	// SQL does not carry a redundant filter.
	if states := q["state"]; len(states) > 0 {
		valid := make(map[storage.State]bool, len(storage.ActiveStates()))
		for _, s := range storage.ActiveStates() {
			valid[s] = true
		}
		for _, p := range states {
			p = strings.TrimSpace(p)
			if valid[storage.State(p)] {
				f.ActiveSubs = append(f.ActiveSubs, p)
			}
		}
	}
	return f
}

func mediaTypeFilter(s string) string {
	switch s {
	case "movie":
		return "movie"
	case "tv":
		return "tv"
	default:
		return ""
	}
}

func sortFilter(s string) string {
	if s == "title" {
		return "title"
	}
	return "date"
}

func sinceFilter(s string) string {
	switch s {
	case "24h", "7d", "30d":
		return s
	default:
		return ""
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	filter := parseDashboardFilter(r)
	af, rf := filter.toStorage(dashboardLimit, 10)

	active, err := s.store.ListRequests(r.Context(), af)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	recent, err := s.store.ListRequests(r.Context(), rf)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "dashboard", dashboardView{
		page:      s.basePage(w, r, "dashboard"),
		Filter:    filter,
		Active:    s.rowsFor(r, active, now),
		Recent:    s.rowsFor(r, recent, now),
		ActiveAll: storage.ActiveStates(),
	})
}

// handleActiveFragment is what the dashboard polls.
func (s *Server) handleActiveFragment(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	filter := parseDashboardFilter(r)
	af, _ := filter.toStorage(20, 0)

	// A shorter list than the page's: this is fetched every three seconds by
	// every open browser, and nobody scrolls a live table.
	active, err := s.store.ListRequests(r.Context(), af)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The response is not cached, and this is the whole reason the fragment
	// exists: a browser that cached it would show a progress bar frozen at
	// whatever it read first.
	w.Header().Set("Cache-Control", "no-store")

	// The page struct travels with the fragment because the fragment contains
	// a form, and a form without a token is rejected at submission — which
	// would look like the cancel button being broken rather than like a
	// missing field.
	s.renderFragment(w, r, "active_table", activeFragmentView{
		page:      s.basePage(w, r, "dashboard"),
		Filter:    filter,
		Active:    s.rowsFor(r, active, now),
		ActiveAll: storage.ActiveStates(),
	})
}

// rowsFor renders requests, attaching the watchers of each.
//
// One query per request, which is fine at this scale and honest about what it
// costs: the alternative is a join that returns a variable number of rows per
// request and has to be regrouped in Go anyway.
func (s *Server) rowsFor(r *http.Request, requests []*storage.Request, now time.Time) []requestRow {
	rows := make([]requestRow, 0, len(requests))
	for _, req := range requests {
		watchers, err := s.store.ListWatchers(r.Context(), req.ID)
		if err != nil {
			s.log.Warn("could not list the watchers of a request",
				"request", req.ID, "error", err)
		}
		rows = append(rows, rowFor(req, watchers, now))
	}
	return rows
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	data := searchView{page: s.basePage(w, r, "search"), Query: query}

	if query != "" {
		results, err := s.tmdb.SearchMulti(r.Context(), query)
		if err != nil {
			// Rendered on the page rather than as a 500: the search box is
			// right there, and a TMDB outage should not look like the
			// application is broken.
			data.Error = searchFailure(err)
			s.render(w, r, "search", data)
			return
		}

		for _, res := range results {
			data.Results = append(data.Results, searchCardFor(res))
		}
	}

	s.render(w, r, "search", data)
}

// searchFailure explains a metadata lookup failure in terms a user can act on.
func searchFailure(err error) string {
	var apiErr *tmdb.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Unauthorized():
			return "TMDB не принял ключ. Проверьте tmdb.api_key."
		case apiErr.RateLimited():
			return "TMDB просит подождать. Попробуйте через минуту."
		case apiErr.NotFound():
			return "TMDB ничего не ответил на этот запрос."
		}
	}
	return "Не удалось спросить TMDB: " + err.Error()
}

// ---------------------------------------------------------------------------
// Add: the pages between a search result and a request
// ---------------------------------------------------------------------------

// handleAdd shows the confirmation for a film, or the season list for a series.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	ref, err := refFromPath(r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if ref.Type == domain.MediaTypeMovie {
		s.showConfirmation(w, r, ref, nil)
		return
	}

	show, err := s.tmdb.TV(r.Context(), ref.TMDBID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	now := time.Now()
	seasons := make([]seasonCard, 0, len(show.Seasons))
	for _, season := range show.Seasons {
		card := seasonCard{
			Number:   season.Number,
			Episodes: season.EpisodeCount,
		}
		if season.AirDate != nil {
			card.AirDate = season.AirDate.Format("2006-01-02")
			card.Upcoming = season.AirDate.After(now)
		}
		seasons = append(seasons, card)
	}

	s.render(w, r, "seasons", seasonsView{
		page: s.basePage(w, r, "search"),
		Show: showHeader{
			TMDBID:    show.Ref.TMDBID,
			Title:     show.Title,
			Year:      show.Year,
			PosterURL: posterPath(show.PosterPath, "w342"),
		},
		Seasons: seasons,
	})
}

// handleAddSeason shows the confirmation for one season of a series.
func (s *Server) handleAddSeason(w http.ResponseWriter, r *http.Request) {
	ref, err := refFromPath(r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	season, err := strconv.Atoi(r.PathValue("season"))
	if err != nil || season < 0 {
		http.Error(w, "сезон должен быть числом", http.StatusBadRequest)
		return
	}

	s.showConfirmation(w, r, ref, &season)
}

// showConfirmation loads a title and renders the last page before a request.
func (s *Server) showConfirmation(w http.ResponseWriter, r *http.Request, ref domain.MediaRef, season *int) {
	spec, posterPath, err := s.loadSpec(r, ref, season)
	if err != nil {
		// A season that does not exist is a bad link, not a server fault.
		if errors.Is(err, errNoSeason) || errors.Is(err, errNotFoundTitle) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "confirm", confirmView{
		page:         s.basePage(w, r, "search"),
		confirmation: confirmationFor(spec, posterPath),
	})
}

// ---------------------------------------------------------------------------
// Creating a request
// ---------------------------------------------------------------------------

func (s *Server) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	kind := r.PostFormValue("media_type")
	tmdbID, err := strconv.ParseInt(r.PostFormValue("tmdb_id"), 10, 64)
	if err != nil || tmdbID <= 0 {
		http.Error(w, "неверный tmdb id", http.StatusBadRequest)
		return
	}

	ref := domain.MovieRef(tmdbID)
	if kind == string(domain.MediaTypeTV) {
		ref = domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: tmdbID}

		season, err := strconv.Atoi(r.PostFormValue("season"))
		if err != nil || season < 0 {
			http.Error(w, "для сериала нужен сезон", http.StatusBadRequest)
			return
		}
		ref.Season = &season
	}

	// The metadata is re-read from TMDB rather than taken from the form. The
	// form carries an identity and nothing else: a client that could post the
	// title, the year and the release date could post a wrong one, and this
	// spec is frozen into the workflow for two years.
	spec, _, err := s.loadSpec(r, ref, ref.Season)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	user := sessionUser(r)

	res, err := s.req.StartRequest(r.Context(), request.StartInput{
		Spec:   spec,
		UserID: user.ID,
	})
	if err != nil {
		s.log.Error("could not start a request", "media", spec.String(), "error", err)
		s.renderError(w, r, "search", "Не удалось поставить запрос: "+err.Error())
		return
	}

	if res.Reused {
		s.flash(w, r, fmt.Sprintf("«%s» уже ищется — вы подписаны на уведомления.", spec.DisplayTitle()), "ok")
	} else {
		s.flash(w, r, fmt.Sprintf("Запрос на «%s» создан.", spec.DisplayTitle()), "ok")
	}

	s.log.Info("request created from the web interface",
		"request", res.Request.ID, "media", spec.String(),
		"user", user.Username, "reused", res.Reused)

	s.seeOther(w, r, fmt.Sprintf("/requests/%d", res.Request.ID))
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	state := r.URL.Query().Get("state")

	filter := storage.RequestFilter{Limit: 200}
	switch state {
	case "", "active":
		filter.States = storage.ActiveStates()
	case "all":
		// No filter at all.
	default:
		candidate := storage.State(state)
		if !candidate.Known() {
			http.Error(w, "неизвестное состояние: "+state, http.StatusBadRequest)
			return
		}
		filter.States = []storage.State{candidate}
	}

	requests, err := s.store.ListRequests(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	all := make([]string, 0, len(storage.AllStates()))
	for _, st := range storage.AllStates() {
		all = append(all, string(st))
	}

	s.render(w, r, "requests", requestsView{
		page:     s.basePage(w, r, "dashboard"),
		State:    state,
		States:   all,
		Requests: s.rowsFor(r, requests, now),
	})
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "неверный id", http.StatusBadRequest)
		return
	}

	req, err := s.store.GetRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, r, err)
		return
	}

	watchers, err := s.store.ListWatchers(r.Context(), id)
	if err != nil {
		s.log.Warn("could not list watchers", "request", id, "error", err)
	}

	// Candidates are loaded separately from the projection because they
	// live in a different table — request_candidates — and the list
	// can be large. Showing them only on the detail page is enough.
	candidates, err := s.store.ListCandidates(r.Context(), id)
	if err != nil {
		s.log.Warn("could not list candidates", "request", id, "error", err)
	}

	events, err := s.store.ListEvents(r.Context(), id, 30)
	if err != nil {
		s.log.Warn("could not list events", "request", id, "error", err)
	}

	s.render(w, r, "request", requestView{
		page:       s.basePage(w, r, "dashboard"),
		Request:    detailFor(req, watchers, events, time.Now()),
		Candidates: candidates,
	})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "неверный id", http.StatusBadRequest)
		return
	}

	if err := s.req.Cancel(r.Context(), id); err != nil {
		s.log.Error("could not cancel a request", "request", id, "error", err)
		s.flash(w, r, "Не удалось отменить: "+err.Error(), "bad")
		s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
		return
	}

	s.flash(w, r, "Отмена запрошена. Торрент будет удалён, если он был добавлен.", "ok")
	s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "неверный id", http.StatusBadRequest)
		return
	}

	user := sessionUser(r)

	if _, err := s.req.Retry(r.Context(), request.RetryInput{
		RequestID:   id,
		Actor:       "api",
		RequestedBy: user.Username,
	}); err != nil {
		s.log.Error("could not retry a request", "request", id, "error", err)
		s.flash(w, r, "Не удалось перезапустить: "+err.Error(), "bad")
		s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
		return
	}

	s.flash(w, r, "Запрос запущен снова: поиск начнётся с текущего момента.", "ok")
	s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)

	// Only the unread inbox. Read ones are kept in the table so the
	// operator can still inspect the history, but the page itself is
	// "what's left to attend to", and a click on "Mark as read" should
	// visibly remove the row.
	items, err := s.store.ListNotifications(r.Context(), user.ID, true, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// One query for all posters: notifications with request_id=0 (admin
	// alerts) come back without a key, and the template falls back to a
	// placeholder.
	requestIDs := make([]int64, 0, len(items))
	for _, n := range items {
		if n.RequestID > 0 {
			requestIDs = append(requestIDs, n.RequestID)
		}
	}
	posters, err := s.store.MediaPostersByRequestIDs(r.Context(), requestIDs)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	rows := make([]notificationRow, 0, len(items))
	for _, n := range items {
		rows = append(rows, notificationRow{
			ID:        n.ID,
			RequestID: n.RequestID,
			Title:     n.Title,
			Body:      n.Body,
			When:      n.CreatedAt.Format("2006-01-02 15:04"),
			Unread:    false,
			PosterURL: posterPath(posters[n.RequestID], "w92"),
		})
	}

	// The unread flag is not part of the storage row, so it is derived here
	// from the same query the badge uses rather than guessed.
	unread, err := s.store.ListNotifications(r.Context(), user.ID, true, 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	unreadIDs := make(map[int64]bool, len(unread))
	for _, n := range unread {
		unreadIDs[n.ID] = true
	}
	for i := range rows {
		rows[i].Unread = unreadIDs[rows[i].ID]
	}

	s.render(w, r, "notifications", notificationsView{
		page:  s.basePage(w, r, "notifications"),
		Items: rows,
	})
}

func (s *Server) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	user := sessionUser(r)

	raw := r.PostForm["id"]
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}

	if err := s.store.MarkNotificationsRead(r.Context(), user.ID, ids); err != nil {
		s.fail(w, r, err)
		return
	}

	s.seeOther(w, r, "/notifications")
}

// ---------------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------------

// handleHelp serves the documentation in views/help.html.
//
// Public: a new user should be able to read what the bot does
// before signing in. The page is static — no DB calls, no query
// parameters — and the layout already handles a nil user
// (the username/logout block is hidden). The route is registered
// outside the requireAuth group at module.go for the same reason.
func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "help", helpView{page: s.basePage(w, r, "help")})
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)

	var (
		invites []inviteRow
		users   []storage.User
	)

	if user.IsAdmin {
		stored, err := s.store.ListInvites(r.Context(), 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, inv := range stored {
			row := inviteRow{
				Code:      inv.Code,
				CreatedAt: inv.CreatedAt.Format("2006-01-02"),
				ExpiresAt: inv.ExpiresAt.Format("2006-01-02"),
				State:     "действует",
			}
			switch {
			case inv.UsedBy != nil:
				row.State = "использован"
			case time.Now().After(inv.ExpiresAt):
				row.State = "истёк"
			}
			invites = append(invites, row)
		}

		if users, err = s.store.ListUsers(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}

	linked, err := s.store.ListTelegramIdentities(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tg := make([]telegramRow, 0, len(linked))
	for _, id := range linked {
		tg = append(tg, telegramRow{
			ID:        id.ExternalID,
			CreatedAt: id.CreatedAt.Format("2006-01-02"),
		})
	}

	mask, err := s.store.GetTelegramNotifyMask(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	opts := []notificationOption{
		{Bit: 1, Name: "started", Label: "Поиск начался"},
		{Bit: 2, Name: "completed", Label: "Загрузка завершилась"},
		{Bit: 4, Name: "failed", Label: "Загрузка провалилась"},
		// Bit 8 covers two kinds on the watcher side: "skipped"
		// (request was already in Plex) and "manual" (the
		// workflow is waiting for you to pick a release from a
		// candidate list). They share the same bit because the
		// user-facing decision is the same: do you want the bot
		// to bother you about an out-of-band result?
		{Bit: 8, Name: "skipped_and_manual", Label: "Уже было в Plex или пора выбрать релиз"},
	}
	for i := range opts {
		opts[i].Checked = mask&opts[i].Bit != 0
	}

	tab := r.URL.Query().Get("tab")
	switch tab {
	case "account", "telegram", "notifications", "invites", "users":
	default:
		tab = "account"
	}

	newInvite := r.URL.Query().Get("new_invite")
	var newInviteURL string
	if newInvite != "" {
		// The full URL the admin hands to the invitee. Built server-side
		// from the configured base URL and the request's base path, so
		// /mediad/join works for any deployment that mounts under a
		// prefix.
		newInviteURL = strings.TrimRight(s.cfg.Server.BaseURL, "/") +
			s.path("/join?code="+url.QueryEscape(newInvite))
	}

	s.render(w, r, "settings", settingsView{
		page:          s.basePage(w, r, "settings"),
		Tab:           tab,
		Invites:       invites,
		Users:         users,
		InviteTTL:     humanDuration(storage.InviteTTL),
		NewInvite:     newInvite,
		NewInviteURL:  newInviteURL,
		Telegram:      tg,
		BotUsername:   s.cfg.Telegram.BotUsername,
		NotifyOptions: opts,
	})
}

// handleTelegramSettings is the form target for "link" and "unlink".
//
// The action is in the form's "action" field rather than the route, so a
// single endpoint can serve two related operations without two routes and
// two CSRF-protected forms.
func (s *Server) handleTelegramSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	user := sessionUser(r)
	action := r.PostFormValue("action")

	switch action {
	case "link":
		code, err := s.store.CreateLinkCode(r.Context(), user.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.flash(w, r, "Код привязки: "+code+". Откройте бота и отправьте /link "+code+".", "ok")
	case "unlink":
		telegramID := r.PostFormValue("telegram_id")
		if telegramID == "" {
			http.Error(w, "не указан telegram id", http.StatusBadRequest)
			return
		}
		removed, err := s.store.UnlinkTelegram(r.Context(), user.ID, telegramID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if removed {
			s.flash(w, r, "Telegram-аккаунт отвязан", "ok")
		} else {
			s.flash(w, r, "Этот Telegram-аккаунт не был привязан к вам", "warn")
		}
	default:
		http.Error(w, "неизвестное действие", http.StatusBadRequest)
		return
	}
	s.seeOther(w, r, "/settings")
}

// handleNotificationSettings records the Telegram notify mask.
func (s *Server) handleNotificationSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	user := sessionUser(r)
	mask := 0
	for _, opt := range []struct {
		name string
		bit  int
	}{
		{"started", 1}, {"completed", 2}, {"failed", 4}, {"skipped", 8},
	} {
		if r.PostFormValue(opt.name) != "" {
			mask |= opt.bit
		}
	}
	if err := s.store.SetTelegramNotifyMask(r.Context(), user.ID, mask); err != nil {
		s.fail(w, r, err)
		return
	}
	s.flash(w, r, "Настройки уведомлений сохранены", "ok")
	s.seeOther(w, r, "/settings")
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	user := sessionUser(r)

	// The current password is required even though the session already proves
	// identity: an unattended browser is the threat, and this is the one form
	// whose effect is to lock the owner out of their own account.
	if _, err := s.store.Authenticate(r.Context(), user.Username, r.PostFormValue("current")); err != nil {
		s.flash(w, r, "Текущий пароль неверен.", "bad")
		s.seeOther(w, r, "/settings")
		return
	}

	password := r.PostFormValue("password")
	if err := storage.ValidatePassword(password); err != nil {
		s.flash(w, r, err.Error(), "bad")
		s.seeOther(w, r, "/settings")
		return
	}

	if err := s.store.SetPassword(r.Context(), user.ID, password); err != nil {
		s.fail(w, r, err)
		return
	}

	// Every session goes, including this one. A password change that leaves
	// other sessions alive does not do the one thing it is for.
	if err := s.store.DeleteUserSessions(r.Context(), user.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.clearCookie(w, sessionCookie, r)

	s.log.Info("password changed", "user", user.Username)
	s.flash(w, r, "Пароль изменён. Войдите заново.", "ok")
	s.seeOther(w, r, "/login")
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)
	if !user.IsAdmin {
		http.Error(w, "нужны права администратора", http.StatusForbidden)
		return
	}

	code, err := s.store.CreateInvite(r.Context(), user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.log.Info("invite created", "by", user.Username)
	// Pass the code through the URL rather than a flash cookie: a clickable
	// link survives a refresh, and the cookie-based flash is one-shot.
	s.seeOther(w, r, fmt.Sprintf("/settings?tab=invites&new_invite=%s", url.QueryEscape(code)))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Errors the confirmation path distinguishes from a server fault.
var (
	errNoSeason      = errors.New("season not found")
	errNotFoundTitle = errors.New("title not found")
)

// loadSpec resolves a reference into the spec a request is created from.
//
// The poster path is returned alongside because it is on the same response and
// re-fetching it for the confirmation page would be a second round trip for a
// cosmetic field.
func (s *Server) loadSpec(r *http.Request, ref domain.MediaRef, season *int) (domain.MediaSpec, string, error) {
	if err := ref.Valid(); err != nil {
		return domain.MediaSpec{}, "", err
	}

	if ref.Type == domain.MediaTypeMovie {
		title, err := s.tmdb.Movie(r.Context(), ref.TMDBID)
		if err != nil {
			return domain.MediaSpec{}, "", err
		}
		if title == nil {
			return domain.MediaSpec{}, "", fmt.Errorf("%w: %s", errNotFoundTitle, ref)
		}
		return title.ToSpec(), title.PosterPath, nil
	}

	show, err := s.tmdb.TV(r.Context(), ref.TMDBID)
	if err != nil {
		return domain.MediaSpec{}, "", err
	}
	if show == nil {
		return domain.MediaSpec{}, "", fmt.Errorf("%w: %s", errNotFoundTitle, ref)
	}

	if season == nil {
		return domain.MediaSpec{}, "", fmt.Errorf("%w: a series needs a season", errNoSeason)
	}

	for _, candidate := range show.Seasons {
		if candidate.Number != *season {
			continue
		}

		spec := show.ToSpec()
		spec.Ref = domain.SeasonRef(show.Ref.TMDBID, candidate.Number)
		// The season's own première, not the series'. A request carrying the
		// series' date would look like media released years ago, and the
		// workflow would search for a season that has not aired.
		if candidate.AirDate != nil {
			spec.ReleasedAt = candidate.AirDate
		}

		poster := candidate.PosterPath
		if poster == "" {
			poster = show.PosterPath
		}
		return spec, poster, nil
	}

	return domain.MediaSpec{}, "", fmt.Errorf("%w: %s has no season %d", errNoSeason, show.Title, *season)
}

// refFromPath reads a media reference out of the URL.
func refFromPath(kind, id string) (domain.MediaRef, error) {
	tmdbID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || tmdbID <= 0 {
		return domain.MediaRef{}, fmt.Errorf("неверный tmdb id %q", id)
	}

	switch kind {
	case "movie":
		return domain.MovieRef(tmdbID), nil
	case "tv":
		return domain.MediaRef{Type: domain.MediaTypeTV, TMDBID: tmdbID}, nil
	default:
		return domain.MediaRef{}, fmt.Errorf("неизвестный тип %q", kind)
	}
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// maxTorrentUpload bounds an uploaded .torrent file.
//
// A torrent file is metadata: a few kilobytes for a film, a megabyte or two
// for a release with thousands of files. Anything larger is not a torrent
// file, and reading it into memory would be the wrong way to find that out.
const maxTorrentUpload = 4 << 20

// handleManualRelease accepts a user-submitted release.
//
// The release arrives either as a magnet link or as an uploaded .torrent
// file. The file is turned into a magnet here, so that everything downstream
// — the workflow, the audit trail, the download client — only ever deals
// with links.
//
// The link is stored on the request row in selected_link; the workflow picks
// it up on its next poll and calls AddTorrent with it. The workflow is the
// only place that can actually start the download — running qBittorrent's
// AddTorrent here would race with the workflow's own add and leave two rows
// tagged with the same request id.
func (s *Server) handleManualRelease(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	id, err := pathID(r)
	if err != nil {
		http.Error(w, "неверный id", http.StatusBadRequest)
		return
	}

	link := strings.TrimSpace(r.PostFormValue("link"))
	uploaded := false

	// A file wins over the text field: a browser sends the text field
	// prefilled with the link that is already attached, so the field is
	// rarely empty and never says what the user just picked.
	if file := uploadedTorrent(r); file != nil {
		magnet, err := magnetFromTorrent(file)
		if err != nil {
			s.flash(w, r, "Файл не похож на торент: "+err.Error(), "bad")
			s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
			return
		}
		link = magnet
		uploaded = true
	}

	if err := s.store.SetSelectedLink(r.Context(), id, link); err != nil {
		s.flash(w, r, "Ссылка не подходит: "+err.Error(), "bad")
		s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
		return
	}

	switch {
	case link == "":
		s.flash(w, r, "Ручной релиз снят.", "ok")
	case uploaded:
		s.flash(w, r, "Файл принят. Workflow подхватит его на ближайшем тике.", "ok")
	default:
		s.flash(w, r, "Ссылка сохранена. Workflow подхватит её на ближайшем тике.", "ok")
	}
	s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
}

// uploadedTorrent returns the .torrent file the request carries, if any.
//
// A file input with nothing chosen sends an empty part, which the multipart
// parser treats as a value rather than a file — so an untouched input does
// not show up here.
func uploadedTorrent(r *http.Request) *multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	files := r.MultipartForm.File["torrent"]
	if len(files) == 0 {
		return nil
	}
	return files[0]
}

// magnetFromTorrent reads an uploaded .torrent file and returns the magnet
// URI it describes.
//
// The magnet carries the announce URLs from the file, which is what makes it
// work on a private tracker: without them the client has no way in but the
// DHT, and a private tracker is not on the DHT.
func magnetFromTorrent(fh *multipart.FileHeader) (string, error) {
	if fh.Size > maxTorrentUpload {
		return "", fmt.Errorf("the file is %d bytes, over the %d byte limit", fh.Size, maxTorrentUpload)
	}

	f, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("open the uploaded file: %w", err)
	}
	defer f.Close()

	// Read one byte past the limit, so a file that lies about its size is
	// caught here rather than truncated into a torrent that parses.
	data, err := io.ReadAll(io.LimitReader(f, maxTorrentUpload+1))
	if err != nil {
		return "", fmt.Errorf("read the uploaded file: %w", err)
	}
	if len(data) > maxTorrentUpload {
		return "", fmt.Errorf("the file is over the %d byte limit", maxTorrentUpload)
	}

	magnet, err := torrentfile.MagnetURI(data)
	if err != nil {
		return "", err
	}
	return magnet, nil
}

// handlePickCandidate marks a candidate as picked and wakes the request.
//
// The choice is written to the database first; the signal that follows only
// tells the workflow that there is something to look at. The signal is what
// makes the choice take effect in seconds rather than at the request's next
// scheduled tick, which for a request deep in its backoff ladder is a day
// away.
//
// A signal that does not arrive is not a lost choice: the pick is in the
// database, and the workflow reads it on its next tick whatever happens. So
// a failure here is logged and the user is told the request will pick it up
// shortly — never that the choice was lost.
func (s *Server) handlePickCandidate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	id, err := pathID(r)
	if err != nil {
		http.Error(w, "неверный id", http.StatusBadRequest)
		return
	}

	rank, err := strconv.Atoi(r.PostFormValue("rank"))
	if err != nil || rank <= 0 {
		http.Error(w, "неверный rank", http.StatusBadRequest)
		return
	}

	if _, err := s.store.MarkPicked(r.Context(), id, rank); err != nil {
		if errors.Is(err, storage.ErrNoSuchCandidate) {
			http.Error(w, "такого кандидата нет (rank="+strconv.Itoa(rank)+")", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}

	if err := s.req.SignalPick(r.Context(), id); err != nil {
		s.log.Warn("could not wake the workflow for a picked candidate",
			"request", id, "rank", rank, "error", err)
		s.flash(w, r, "Выбрано. Workflow подхватит на ближайшем тике.", "ok")
		s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
		return
	}

	s.flash(w, r, "Выбрано. Загрузка начнётся через несколько секунд.", "ok")
	s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
}

// handleEditYear records a corrected year for the request's media row.
//
// A year fix is metadata-only: the request keeps running with its
// existing deadline and search, and the next time the row is rendered
// the new year shows up. The workflow does not need to know.
func (s *Server) handleEditYear(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "не разобрать форму", http.StatusBadRequest)
		return
	}

	id, err := pathID(r)
	if err != nil {
		http.Error(w, "не верный id", http.StatusBadRequest)
		return
	}

	yearStr := strings.TrimSpace(r.PostFormValue("year"))
	year, err := strconv.Atoi(yearStr)
	if err != nil || year < 1800 || year > 2200 {
		s.flash(w, r, "Год должен быть числом между 1800 и 2200.", "bad")
		s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
		return
	}

	req, err := s.store.GetRequest(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.SetMediaYear(r.Context(), req.MediaID, year); err != nil {
		s.fail(w, r, err)
		return
	}
	s.flash(w, r, "Год обновлён.", "ok")
	s.seeOther(w, r, fmt.Sprintf("/requests/%d", id))
}
