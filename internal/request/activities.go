package request

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/indexer"
	"github.com/alex/mediad/internal/plex"
	"github.com/alex/mediad/internal/qbit"
	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// Activities holds the dependencies every activity needs.
//
// It is a struct rather than a set of free functions because the worker
// registers the whole thing in one call, and because the dependencies are the
// same for all of them.
type Activities struct {
	store   *storage.Store
	plex    *plex.Client
	indexer indexer.Indexer
	qbit    *qbit.Client
	profile release.Profile
	cfg     *config.Config
	log     *slog.Logger
}

// NewActivities builds the activity set.
func NewActivities(
	store *storage.Store,
	plexClient *plex.Client,
	idx indexer.Indexer,
	torrents *qbit.Client,
	profile release.Profile,
	cfg *config.Config,
	log *slog.Logger,
) *Activities {
	return &Activities{
		store:   store,
		plex:    plexClient,
		indexer: idx,
		qbit:    torrents,
		profile: profile,
		cfg:     cfg,
		log:     log.With("component", "activities"),
	}
}

// ---------------------------------------------------------------------------
// Plex
// ---------------------------------------------------------------------------

// CheckPlex reports whether the library already has the media.
//
// The answer is cached on the media row with a timestamp, and a cached answer
// is reused until it goes stale. That matters more than it looks: the search
// loop would otherwise ask Plex on every pass, and "has it appeared by other
// means" is not a question whose answer changes hourly.
//
// A negative answer is cached too, and the distinction between "asked and
// absent" and "never asked" is stored — the loop re-checks on the timestamp,
// not on a guess.
func (a *Activities) CheckPlex(ctx context.Context, in CheckPlexInput) (CheckPlexResult, error) {
	mediaID, err := a.store.MediaIDForRequest(ctx, in.RequestID)
	if err != nil {
		return CheckPlexResult{}, err
	}

	media, err := a.store.GetMedia(ctx, mediaID)
	if err != nil {
		return CheckPlexResult{}, err
	}

	if !in.Force && media.PlexCheckedAt != nil && in.MaxAge > 0 {
		if age := time.Since(*media.PlexCheckedAt); age < in.MaxAge {
			return CheckPlexResult{
				InPlex:  media.InPlex,
				Title:   media.PlexTitle,
				Year:    media.PlexYear,
				Checked: false,
			}, nil
		}
	}

	if !a.plex.Configured() {
		// Not an error. A project without Plex configured cannot skip
		// anything, which is a degraded but workable mode — and failing here
		// would make the workflow unable to run at all.
		a.log.Warn("plex is not configured; treating every title as absent")
		return CheckPlexResult{}, nil
	}

	match, err := a.findInPlex(ctx, in.Spec)
	if err != nil {
		return CheckPlexResult{}, err
	}

	var stored *storage.PlexMatch
	if match != nil {
		stored = &storage.PlexMatch{
			RatingKey:  match.RatingKey,
			SectionKey: match.SectionKey,
			Title:      match.Title,
			Year:       match.Year,
			AddedAt:    match.AddedAt,
		}
	}

	if err := a.store.PlexStatus(ctx, media.ID, stored); err != nil {
		// Recording the answer is a cache write; the answer itself is still
		// good. Losing the cache costs a repeated query, not a wrong decision.
		a.log.Warn("could not record the plex answer", "media", media.ID, "error", err)
	}

	if match == nil {
		return CheckPlexResult{Checked: true}, nil
	}
	return CheckPlexResult{
		InPlex:  true,
		Title:   match.Title,
		Year:    match.Year,
		Checked: true,
	}, nil
}

// findInPlex looks the media up in the library.
//
// A film is matched directly; a season is matched by finding its show first,
// because Plex records the season number on the show's children but no
// external guid on the children themselves.
//
// Matching is by title and year, never by the tmdb:// guid. A real Plex
// library has a long tail of items the user never tagged, and the
// application has to recognise those too — a film the user already has but
// the system insists on downloading again is a worse failure than the
// occasional title-collision that the year anchor does not catch.
func (a *Activities) findInPlex(ctx context.Context, spec domain.MediaSpec) (*plex.Match, error) {
	candidates := plexLookupTitles(spec)

	if spec.Ref.IsSeason() {
		season := spec.Ref.SeasonNumber()
		return a.plex.FindSeasonByTitle(ctx, candidates, spec.Year, season)
	}
	return a.plex.FindMovieByTitle(ctx, candidates, spec.Year)
}

// plexLookupTitles is the candidate list the Plex lookup compares against.
//
// The order matters: the first entry is the one sent to Plex as a ?title=
// prefilter, and the smaller that prefilter's result, the less work the
// in-Go comparison has. TitleRU is the most likely Plex entry for a
// Russian-speaking user, and is also the one the spec holds first.
func plexLookupTitles(spec domain.MediaSpec) []string {
	out := make([]string, 0, 3)
	if spec.TitleRU != "" {
		out = append(out, spec.TitleRU)
	}
	if spec.Title != "" && spec.Title != spec.TitleRU {
		out = append(out, spec.Title)
	}
	if spec.OriginalTitle != "" && spec.OriginalTitle != spec.TitleRU && spec.OriginalTitle != spec.Title {
		out = append(out, spec.OriginalTitle)
	}
	return out
}

// RefreshPlex asks the server to rescan one directory.
//
// The path is what makes this affordable: without it Plex rescans the whole
// library, which on a large one takes minutes of disk thrashing after every
// completed download.
func (a *Activities) RefreshPlex(ctx context.Context, in RefreshPlexInput) error {
	if !a.plex.Configured() {
		return nil
	}

	section, err := a.plex.FindSectionFor(ctx, in.Spec.Ref.Type)
	if err != nil {
		return err
	}
	if section == nil {
		return fmt.Errorf("no plex section for %s", in.Spec.Ref.Type)
	}

	a.log.Info("asking plex to rescan",
		"section", section.Key, "path", in.Path, "media", in.Spec.String())

	return a.plex.RefreshSection(ctx, section.Key, in.Path)
}

// ---------------------------------------------------------------------------
// Indexer
// ---------------------------------------------------------------------------

// SearchReleases queries the indexer and picks a release.
//
// The selection itself is a pure function in the release package, called from
// here rather than from the workflow. That is what makes it safe to change:
// editing the scoring, the marker tables or the parser inside a workflow would
// break the replay of every request already running, and those last two years.
func (a *Activities) SearchReleases(ctx context.Context, in SearchInput) (SearchResult, error) {
	log := a.log.With("request", in.RequestID, "media", in.Spec.Ref.String())

	req := indexer.ForMedia(in.Spec, categoriesFor(in.Spec), in.Limit)

	// Ask for the release before its release date and there will be nothing;
	// the workflow already avoids this, and this is a second guard for the
	// case where the date was filled in after the request started.
	now := time.Now()
	if in.Spec.ReleasedAt != nil && in.Spec.ReleasedAt.After(now) {
		return SearchResult{
			Summary: fmt.Sprintf("not released until %s", in.Spec.ReleasedAt.Format(time.DateOnly)),
		}, nil
	}

	releases, err := a.indexer.Search(ctx, req)
	if err != nil {
		// An indexer that refused the request must not be retried in a tight
		// loop: that is how an IP gets banned rather than merely throttled.
		if isRateLimited(err) {
			return SearchResult{}, temporal.NewNonRetryableApplicationError(
				"the indexer refused the request; backing off", "IndexerRateLimited", err)
		}
		return SearchResult{}, fmt.Errorf("search indexer: %w", err)
	}

	if len(releases) == 0 {
		return SearchResult{Summary: "the indexer returned nothing"}, nil
	}

	identities := make([]string, 0, len(releases))
	for _, r := range releases {
		identities = append(identities, r.Identity())
	}

	blacklist, err := a.store.IsBlacklisted(ctx, identities)
	if err != nil {
		// A blacklist that cannot be read must not fail the search: the worst
		// case is picking a release that was already found wanting, which the
		// download loop will catch again.
		log.Warn("could not read the release blacklist", "error", err)
		blacklist = map[string]bool{}
	}

	verdict := release.NewSelector(a.profile).Select(in.Spec, releases, blacklist, in.OngoingThread)

	result := SearchResult{
		Considered: verdict.Considered,
		Summary:    verdict.Summary(),
	}
	for _, rej := range verdict.Rejected {
		result.Rejections = append(result.Rejections, Rejection{
			Title:  rej.Release.Title,
			Reason: string(rej.Reason),
			Detail: rej.Detail,
		})
	}

	if !verdict.Acceptable {
		log.Info("nothing acceptable in the results", "considered", verdict.Considered,
			"summary", verdict.Summary())
		return result, nil
	}

	log.Info("release selected", "title", verdict.Chosen.Title,
		"score", verdict.Score, "audio", verdict.Audio.Describe())

	chosen := verdict.Chosen
	result.Chosen = &chosen
	return result, nil
}

// FetchCandidates runs the same indexer search and selector pass as
// SearchReleases and returns every release the selector rejected, with
// the reason. Used by the manual-pick flow when the search deadline is
// still in the future but nothing the auto-selector would accept
// turned up.
//
// The cost is two round-trips per manual entry: this activity runs
// the indexer again, on top of the SearchReleases call that already
// returned empty. Indexer caching keeps the second one cheap; the
// alternative (a state field carrying the full list through workflow
// events) bloats the event history for every request, not just the
// ones that go manual, and the wasted cycles are negligible next to the
// rest of a request's lifetime.
func (a *Activities) FetchCandidates(ctx context.Context, in SearchInput) ([]storage.Candidate, error) {
	log := a.logOrDefault().With("request", in.RequestID, "media", in.Spec.Ref.String())

	req := indexer.ForMedia(in.Spec, categoriesFor(in.Spec), in.Limit)
	releases, err := a.indexer.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search indexer: %w", err)
	}
	if len(releases) == 0 {
		return nil, nil
	}

	identities := make([]string, 0, len(releases))
	for _, r := range releases {
		identities = append(identities, r.Identity())
	}
	blacklist, err := a.store.IsBlacklisted(ctx, identities)
	if err != nil {
		// A blacklist that cannot be read must not fail the search: the
		// worst case is picking a release that was already found wanting,
		// which the download loop will catch again.
		log.Warn("could not read the release blacklist", "error", err)
		blacklist = map[string]bool{}
	}

	verdict := release.NewSelector(a.profile).Select(in.Spec, releases, blacklist, in.OngoingThread)
	log.Info("fetched candidates for manual pick",
		"considered", verdict.Considered, "rejected", len(verdict.Rejected))

	out := make([]storage.Candidate, 0, len(verdict.Rejected))
	for i, r := range verdict.Rejected {
		reason := string(r.Reason)
		if r.Detail != "" {
			reason = reason + ": " + r.Detail
		}
		out = append(out, storage.Candidate{
			Rank:            i + 1,
			Title:           r.Release.Title,
			Indexer:         r.Release.Indexer,
			InfoHash:        r.Release.InfoHash,
			MagnetLink:      r.Release.MagnetURI,
			SourceURL:       r.Release.DownloadURL,
			SizeBytes:       r.Release.SizeBytes,
			Seeders:         r.Release.Seeders,
			Leechers:        r.Release.Leechers,
			RejectionReason: reason,
		})
	}
	return out, nil
}

// logOrDefault returns a.log or a discard logger if a.log is nil.
//
// Activity methods all need a logger; some are registered in tests
// with a zero-value Activities struct (no log). Returning a discard
// logger instead of panicking keeps the tests passing through and
// means the new manual-mode activities don't have to special-case
// the no-log path themselves.
func (a *Activities) logOrDefault() *slog.Logger {
	if a.log != nil {
		return a.log
	}
	return slog.New(slog.DiscardHandler)
}

// categoriesFor returns the indexer categories worth searching.
//
// These are the standard Torznab category numbers: 2000 is Films, 5000 is TV.
// Asking for the narrow category keeps a search for a film from returning the
// soundtrack and the audiobook.
func categoriesFor(spec domain.MediaSpec) []int {
	if spec.Ref.Type == domain.MediaTypeTV {
		return []int{5000}
	}
	return []int{2000}
}

// ---------------------------------------------------------------------------
// Torrent client
// ---------------------------------------------------------------------------

// AddTorrent hands a release to the client and starts tracking it.
//
// It is idempotent through the tag, which is what makes it safe to retry:
// Temporal re-runs an activity whose result was lost, and a second copy of a
// twenty-gigabyte download is not an acceptable outcome.
func (a *Activities) AddTorrent(ctx context.Context, in AddTorrentInput) (AddTorrentResult, error) {
	tag := qbit.TagFor(in.RequestID)
	link := in.Release.Link()
	if link == "" {
		return AddTorrentResult{}, temporal.NewNonRetryableApplicationError(
			"the selected release has neither a magnet nor a download url",
			"ReleaseNotDownloadable", nil)
	}

	savePath := ""
	switch in.Spec.Ref.Type {
	case domain.MediaTypeMovie:
		savePath = a.cfg.QBittorrent.SavePathMovies
	case domain.MediaTypeTV:
		savePath = a.cfg.QBittorrent.SavePathSeries
	}
	if savePath != "" {
		a.log.Info("using a per-type save path",
			"request", in.RequestID, "type", in.Spec.Ref.Type, "save_path", savePath)
	}

	hash, already, err := a.qbit.Add(ctx, qbit.AddSpec{
		Link:     link,
		Category: "mediad",
		Tags:     []string{tag},
		// Started immediately rather than paused: there is nothing to review
		// between the decision to download and the download.
		Paused:   false,
		SavePath: savePath,
		// qBittorrent's "Download in sequential order" — pieces arrive in
		// order from the start, so the file becomes playable while still
		// being fetched. Wired from config so a deployment that does not
		// want it (older clients, performance-sensitive scenarios) can opt
		// out without a code change.
		SequentialDownload: a.cfg.QBittorrent.SequentialDownload,
		// qBittorrent's "Download first and last pieces first" — header and
		// footer land first so a media player can verify file integrity
		// before the bulk arrives. Same config-driven opt-out.
		FirstLastPiecePrio: a.cfg.QBittorrent.FirstLastPiecePrio,
	})
	if err != nil {
		// A no-space failure is the one case where deleting the torrent
		// would make the problem worse, not better: the request has
		// nothing else to download, and the operator has to free disk
		// space before anything can move. Surface the alert and let the
		// workflow mark the request failed without blacklisting the
		// release.
		if errors.Is(err, qbit.ErrNoSpace) {
			a.log.Error("qBittorrent rejected the torrent: out of disk space",
				"request", in.RequestID, "release", in.Release.Title, "error", err)
			title := "qBittorrent: out of disk space"
			body := fmt.Sprintf("request %d (%s) could not be added because qBittorrent reports no disk space",
				in.RequestID, in.Release.Title)
			if alertErr := a.store.CreateAdminAlert(ctx,
				storage.NotificationFailed, title, body); alertErr != nil {
				// Logged and moved on. The user-facing failure is the
				// "request failed" outcome below; double-logging the alert
				// failure would just add noise to the operator's signal.
				a.log.Warn("could not raise an admin alert for no-space",
					"request", in.RequestID, "error", alertErr)
			}
			// Non-retryable so the workflow goes straight to failed,
			// not back into the search loop.
			return AddTorrentResult{}, temporal.NewNonRetryableApplicationError(
				"qBittorrent is out of disk space", "NoDiskSpace", err)
		}
		return AddTorrentResult{}, fmt.Errorf("add torrent to the client: %w", err)
	}

	if already {
		a.log.Info("torrent was already in the client",
			"request", in.RequestID, "hash", hash, "release", in.Release.Title)
	} else {
		a.log.Info("torrent added",
			"request", in.RequestID, "hash", hash, "release", in.Release.Title)
	}

	return AddTorrentResult{Hash: hash, AlreadyPresent: already}, nil
}

// DeleteTorrent removes a torrent and optionally blacklists its release.
//
// Both happen in one activity on purpose. A torrent removed without being
// blacklisted would be selected again on the very next search, and the request
// would loop forever between the same dead release.
func (a *Activities) DeleteTorrent(ctx context.Context, in DeleteTorrentInput) error {
	if in.BlacklistFor != "" {
		if err := a.store.BlacklistRelease(ctx, in.BlacklistFor, in.Reason); err != nil {
			// Worth failing on: the alternative is re-selecting the same dead
			// release, and the retry policy gives this a few attempts.
			return fmt.Errorf("blacklist release %s: %w", in.BlacklistFor, err)
		}
		a.log.Info("release blacklisted", "infohash", in.BlacklistFor, "reason", in.Reason)
	}

	if in.Hash == "" {
		return nil
	}

	if err := a.qbit.Delete(ctx, in.Hash, in.DeleteFiles); err != nil {
		return fmt.Errorf("delete torrent %s: %w", in.Hash, err)
	}

	a.log.Info("torrent deleted", "request", in.RequestID, "hash", in.Hash,
		"files", in.DeleteFiles)
	return nil
}

// ---------------------------------------------------------------------------
// Download state
// ---------------------------------------------------------------------------

// CheckDownload reports how the current torrent is doing.
//
// It reads the local projection rather than asking the torrent client. The
// client is polled by a single download watcher that writes progress here, and
// the workflow reading its own row is what keeps a two-day download from
// costing a history event per poll — and what keeps the client's session
// cookie used by exactly one caller, given that qBittorrent bans an IP after
// repeated failed logins.
func (a *Activities) CheckDownload(ctx context.Context, in CheckDownloadInput) (CheckDownloadResult, error) {
	p, err := a.store.ReadDownloadProgress(ctx, in.RequestID)
	if err != nil {
		return CheckDownloadResult{}, err
	}

	result := interpretDownload(p, time.Now(), in.StallAfter, in.MissingGrace)

	// Heartbeat so that a cancellation reaches this activity promptly. Without
	// it a cancelled workflow would wait for the full start-to-close timeout.
	activity.RecordHeartbeat(ctx, p.Progress)

	return result, nil
}

// interpretDownload turns what the watcher recorded into what the request
// workflow should do.
//
// It is a pure function so that the mapping can be read and tested in one
// place. The split matters: the watcher writes facts and decides nothing,
// while every decision about what a fact means lives here. Reading the
// activity is then not necessary to know how a stalled download, a failed one
// and a vanished one are treated — and those three differ in a way that is
// easy to get wrong. A torrent that errored is the release's fault, a stalled
// one usually is, and one that vanished without ever starting is the
// infrastructure's, which is why only the first two are remembered as bad.
func interpretDownload(
	p storage.DownloadProgress,
	now time.Time,
	stallAfter time.Duration,
	missingGrace time.Duration,
) CheckDownloadResult {
	result := CheckDownloadResult{
		Hash:        p.QbitHash,
		ContentPath: p.ContentPath,
	}

	switch {
	case p.State == storage.StateCompleted:
		result.State = DownloadDone

	case p.State == storage.StateFailed:
		// The request already ended. Reporting a failure lets the loop exit
		// rather than poll a finished request.
		result.State = DownloadFailed
		result.Detail = p.LastError

	case p.Progress >= 1.0:
		result.State = DownloadDone

	case isClientFailure(p.QbitState):
		// The client has given up, so there is nothing to wait for. The
		// release failed for a reason that will repeat.
		result.State = DownloadFailed
		result.Detail = "the torrent client reported " + p.QbitState
		result.Blacklist = true

	case p.MissingSince != nil && now.Sub(*p.MissingSince) > missingGrace:
		// Gone from the client. Whether the release is to blame depends on how
		// far it got: a torrent that never started was an infrastructure
		// problem and may be retried, while one that got partway and vanished
		// means the release itself is dead.
		result.State = DownloadMissing
		result.Detail = fmt.Sprintf("absent from the torrent client for %s",
			now.Sub(*p.MissingSince).Round(time.Minute))
		result.Blacklist = p.Progress > 0

	case p.ZeroSince != nil && now.Sub(*p.ZeroSince) > stallAfter:
		// Time-based rather than poll-count-based: a count would depend on the
		// poll cadence and would survive a restart differently.
		//
		// This also covers a torrent the client reports as stopped. This
		// project only ever adds torrents running, so a stopped one is a state
		// nobody asked for, and waiting out the two-day deadline for something
		// that is not moving ends in the same deletion, only later.
		result.State = DownloadStalled
		result.Detail = fmt.Sprintf("no progress for %s",
			now.Sub(*p.ZeroSince).Round(time.Minute))
		result.Blacklist = true

	default:
		result.State = DownloadRunning
	}

	return result
}

// isClientFailure reports whether a torrent client state means the torrent is
// not going to complete.
//
// The state strings are qBittorrent's. They are matched here rather than in the
// domain package because this is where the decision is made — the domain type
// exposes Done, Failed and Stalled for the cases that need classifying
// everywhere, and this covers the rest.
func isClientFailure(state string) bool {
	switch state {
	case "error", "missingfiles":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

// SaveState writes the workflow's view of a request and records an event.
//
// Both writes happen in one transaction, so the audit trail cannot end up
// describing a state the row is not in.
func (a *Activities) SaveState(ctx context.Context, in SaveStateInput) error {
	return a.store.SaveRequestState(ctx, in.State, storage.Event{
		RequestID: in.RequestID,
		Event:     in.Event,
		ToState:   in.State.State,
		Actor:     in.Actor,
		Detail:    in.Detail,
	})
}

// NotifyWatchers tells everyone watching a request what happened.
//
// The watcher list is read here rather than passed in, and that is the
// decision that removes a class of races: "user B asks for what user A is
// already downloading" is one INSERT into request_watchers with no signal to
// the running workflow at all. The workflow only ever learns the list by
// asking, so it never has a stale copy of it.
func (a *Activities) NotifyWatchers(ctx context.Context, in NotifyInput) error {
	created, err := a.store.CreateNotifications(ctx, in.RequestID, in.Kind, in.Title, in.Body, in.Payload)
	if err != nil {
		return err
	}

	// Telegram delivery is a separate concern, added with the bot. What
	// matters here is that the rows exist before this activity returns: the
	// in-app inbox is the durable record, and a message that fails to send can
	// be retried from it.
	a.log.Info("notified watchers",
		"request", in.RequestID, "kind", in.Kind, "watchers", len(created))

	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isRateLimited reports whether an indexer error means "back off".
func isRateLimited(err error) bool {
	var idxErr *indexer.Error
	if errors.As(err, &idxErr) {
		return idxErr.RateLimited() || idxErr.Unauthorized()
	}
	return false
}

// PopManualLink returns a user-submitted link, if any, and clears the
// field atomically. Called at the start of each search cycle so the
// workflow picks up a magnet the operator pasted in the web UI without
// having to retry the whole request.
func (a *Activities) PopManualLink(ctx context.Context, requestID int64) (string, error) {
	link, err := a.store.PopSelectedLink(ctx, requestID)
	if err != nil {
		return "", fmt.Errorf("pop manual link: %w", err)
	}
	if link == "" {
		a.log.Debug("no manual link on this tick", "request", requestID)
	} else {
		a.log.Info("picked up a manual link", "request", requestID)
	}
	return link, nil
}

// StoreCandidates replaces the candidate list for a request.
//
// Called when the workflow enters StateManual. The list is also written
// by the indexer pass that the search just made; the persistence is in
// the storage activity rather than the workflow itself because the
// result is large and only the storage needs to carry it across the
// activity boundary.
//
// StoreCandidatesInput rather than (int64, []Candidate) for the same
// reason FetchCandidates takes SearchInput: the slice is large and
// the struct is a single, named argument that survives the activity
// boundary without a positional pairing. The activity body unpacks
// the two fields it actually uses.
func (a *Activities) StoreCandidates(ctx context.Context, in StoreCandidatesInput) error {
	return a.store.StoreCandidates(ctx, in.RequestID, in.Candidates)
}

// ClearCandidates removes the candidate list for a request.
//
// Called when the workflow leaves StateManual — the user picked, the
// 14-day timeout fired, or the request was cancelled. The list would
// be stale after any of those and a re-display would mislead the user.
func (a *Activities) ClearCandidates(ctx context.Context, requestID int64) error {
	return a.store.ClearCandidates(ctx, requestID)
}

// GetPickedCandidate returns the candidate the user picked, or
// ErrNoSuchCandidate if no row has picked_at set.
//
// Polled by the workflow on every tick of the manual-mode loop. The
// partial index on `picked_at IS NOT NULL` keeps the poll at
// index-scan cost even when the candidate list has grown.
//
// The "not picked" signal travels back to the workflow as a
// non-retryable typed ApplicationError rather than as the
// storage's sentinel error. Three things motivate the wrapper:
//  1. A plain `errors.New(...)` does not survive the activity
//     boundary as a typed value — the SDK converts it to a
//     generic ApplicationError, and the workflow's `errors.As`
//     check misses.
//  2. The "not picked" signal is a permanent state, not a
//     transient failure. A retry policy of 10 attempts on a
//     row that does not exist yet costs 10 polls' worth of
//     activity time for no information, and the workflow's
//     poll cadence is the right place to wait.
//  3. Same shape as the qBittorrent NoDiskSpace path: a
//     non-retryable typed ApplicationError that the workflow
//     branches on with `errors.As` + `Type()`.
func (a *Activities) GetPickedCandidate(ctx context.Context, requestID int64) (storage.Candidate, error) {
	c, err := a.store.GetPickedCandidate(ctx, requestID)
	if errors.Is(err, storage.ErrNoSuchCandidate) {
		return storage.Candidate{}, temporal.NewNonRetryableApplicationError(
			storage.ErrNoSuchCandidate.Error(), "NoSuchCandidate", err)
	}
	return c, err
}
