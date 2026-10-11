package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

// dashboardLimit caps the lists on the front page.
const dashboardLimit = 50

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	active, err := s.store.ListRequests(r.Context(), storage.RequestFilter{
		States: storage.ActiveStates(),
		Limit:  dashboardLimit,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	recent, err := s.store.ListRequests(r.Context(), storage.RequestFilter{
		States: []storage.State{
			storage.StateCompleted, storage.StateFailed,
			storage.StateCanceled, storage.StateSkipped,
		},
		Limit: 10,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "dashboard", dashboardView{
		page:   s.basePage(w, r, "dashboard"),
		Active: s.rowsFor(r, active, now),
		Recent: s.rowsFor(r, recent, now),
	})
}

// handleActiveFragment is what the dashboard polls.
func (s *Server) handleActiveFragment(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	// A shorter list than the page's: this is fetched every three seconds by
	// every open browser, and nobody scrolls a live table.
	active, err := s.store.ListRequests(r.Context(), storage.RequestFilter{
		States: storage.ActiveStates(),
		Limit:  20,
	})
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
		page:   s.basePage(w, r, "dashboard"),
		Active: s.rowsFor(r, active, now),
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
		page:    s.basePage(w, r, "search"),
		Show:    showHeader{TMDBID: show.Ref.TMDBID, Title: show.Title, Year: show.Year},
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

	http.Redirect(w, r, fmt.Sprintf("/requests/%d", res.Request.ID), http.StatusSeeOther)
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

	events, err := s.store.ListEvents(r.Context(), id, 30)
	if err != nil {
		s.log.Warn("could not list events", "request", id, "error", err)
	}

	s.render(w, r, "request", requestView{
		page:    s.basePage(w, r, "dashboard"),
		Request: detailFor(req, watchers, events, time.Now()),
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
		http.Redirect(w, r, fmt.Sprintf("/requests/%d", id), http.StatusSeeOther)
		return
	}

	s.flash(w, r, "Отмена запрошена. Торрент будет удалён, если он был добавлен.", "ok")
	http.Redirect(w, r, fmt.Sprintf("/requests/%d", id), http.StatusSeeOther)
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
		http.Redirect(w, r, fmt.Sprintf("/requests/%d", id), http.StatusSeeOther)
		return
	}

	s.flash(w, r, "Запрос запущен снова: поиск начнётся с текущего момента.", "ok")
	http.Redirect(w, r, fmt.Sprintf("/requests/%d", id), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)

	items, err := s.store.ListNotifications(r.Context(), user.ID, false, 100)
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

	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
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

	s.render(w, r, "settings", settingsView{
		page:      s.basePage(w, r, "settings"),
		Invites:   invites,
		Users:     users,
		InviteTTL: humanDuration(storage.InviteTTL),
	})
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
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	password := r.PostFormValue("password")
	if err := storage.ValidatePassword(password); err != nil {
		s.flash(w, r, err.Error(), "bad")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
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
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	s.flash(w, r, "Код приглашения: "+code, "ok")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
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
