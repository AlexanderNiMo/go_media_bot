// Package qbit is a client for qBittorrent's Web API.
//
// It wraps github.com/autobrr/go-qbittorrent, which handles the parts that are
// tedious to get right — the SID cookie session, and re-login when the server
// invalidates it — and adds the three things this project needs on top:
//
//   - Idempotent adds. `torrents/add` returns no hash on most versions, so a
//     retried activity cannot tell whether it already added the torrent.
//     Every torrent this project adds is tagged, and Add looks for the tag
//     before adding anything.
//
//   - Version-aware option names. qBittorrent 5.0 renamed the `paused`
//     parameter to `stopped`; the library passes options through verbatim, so
//     sending the wrong one is silently ignored and the torrent starts
//     downloading when it was meant to wait.
//
//   - A single normalised status type, so the version differences above stop
//     at this package.
package qbit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	qbittorrent "github.com/autobrr/go-qbittorrent"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// Module wires the qBittorrent client into the fx graph.
var Module = fx.Module("qbit", fx.Provide(New))

// ConnectivityModule logs in at startup.
//
// It is separate from Module because not every entry point needs the torrent
// client: `mediad migrate` and the read-only CLI commands must work with
// qBittorrent switched off, and a login attempt would make them fail.
//
// Logging in here rather than lazily means a client that cannot authenticate
// fails at startup with a clear message, instead of at the first download
// after a request has already been waiting for hours.
var ConnectivityModule = fx.Module("qbit-connectivity",
	fx.Invoke(func(lc fx.Lifecycle, c *Client) {
		lc.Append(fx.Hook{OnStart: c.Start})
	}),
)

const (
	// tagPrefix namespaces this project's tags inside the client, so they can
	// be told apart from tags a user or another tool applied.
	tagPrefix = "mediadbot:"

	// hashDiscoveryTimeout bounds how long Add waits for a freshly added
	// torrent to appear in the client's list.
	//
	// It is a wait rather than an instant read because `torrents/add` is
	// asynchronous: it returns as soon as the client has accepted the payload,
	// and the torrent shows up in the list a moment later.
	hashDiscoveryTimeout = 30 * time.Second

	hashPollInterval = time.Second
)

// Client talks to qBittorrent.
//
// It is safe for concurrent use, but the project deliberately funnels every
// call through one goroutine: qBittorrent bans an IP after repeated failed
// logins, and concurrent re-login attempts after a session expiry are exactly
// how that happens.
type Client struct {
	qb      *qbittorrent.Client
	cfg     *config.Config
	log     *slog.Logger
	version string
}

// New builds the client. It does not connect; see ConnectivityModule.
func New(cfg *config.Config, log *slog.Logger) *Client {
	timeout := int(cfg.QBittorrent.PollInterval / time.Second)
	if timeout < 30 {
		timeout = 30
	}

	qb := qbittorrent.NewClient(qbittorrent.Config{
		Host:     cfg.QBittorrent.URL,
		APIKey:   cfg.QBittorrent.APIKey,
		Username: cfg.QBittorrent.Username,
		Password: cfg.QBittorrent.Password,
		Timeout:  timeout,
	})

	return &Client{qb: qb, cfg: cfg, log: log.With("component", "qbit")}
}

// Start logs in and records the server version. It is registered as an fx
// hook so that a client that cannot authenticate fails at startup rather than
// on the first download.
func (c *Client) Start(ctx context.Context) error {
	if c.cfg.QBittorrent.URL == "" {
		return errors.New("qbittorrent.url is not configured")
	}

	if err := c.qb.LoginCtx(ctx); err != nil {
		// The error is worth spelling out: qBittorrent answers 403 to a login
		// from an IP it has banned after too many failures, which reads as
		// "wrong password" but is not.
		return fmt.Errorf("log in to qbittorrent at %s: %w "+
			"(note: a 403 here can also mean this IP is temporarily banned after repeated failed logins)",
			c.cfg.QBittorrent.URL, err)
	}

	v, err := c.qb.GetAppVersionCtx(ctx)
	if err != nil {
		// Fail rather than warn. Pointing this at the wrong port is an easy
		// mistake — 8080 is a very common default — and the failure is silent:
		// an unrelated web service answers the login with a 200, so the client
		// reports a successful connection and then misbehaves on every later
		// call. Refusing to start turns that into one obvious error.
		return fmt.Errorf("read the qbittorrent version from %s: %w "+
			"(is qbittorrent.url pointing at the Web API, not another service?)",
			c.cfg.QBittorrent.URL, err)
	}
	if !looksLikeVersion(v) {
		return fmt.Errorf("the service at %s answered with %q, which is not a qbittorrent version "+
			"(the Web API usually listens on a different port than other services)",
			c.cfg.QBittorrent.URL, truncate(v, 60))
	}
	c.version = v

	c.log.Info("connected to qbittorrent", "url", c.cfg.QBittorrent.URL, "version", v)
	return nil
}

// looksLikeVersion reports whether a string is a plausible qBittorrent version.
//
// The check exists because the add-option name depends on the major version,
// and an unrecognised string silently falls back to the 4.x spelling. A client
// that is not qBittorrent at all returns whatever its own version endpoint
// produces, so validating the shape here is what turns "connected to the wrong
// service" into a startup failure instead of a torrent that quietly starts
// downloading when it was meant to wait.
func looksLikeVersion(v string) bool {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if v == "" {
		return false
	}

	major, rest, found := strings.Cut(v, ".")
	if !found {
		return false
	}
	if _, err := strconv.Atoi(major); err != nil {
		return false
	}
	// The minor component has to start with a digit too; a service returning
	//something like "1.0 (build abc)" would still pass, which is fine.
	minor := rest
	if i := strings.IndexAny(minor, ".-+ "); i >= 0 {
		minor = minor[:i]
	}
	_, err := strconv.Atoi(minor)
	return err == nil
}

// Version returns the server version string, or "" if it could not be read.
func (c *Client) Version() string { return c.version }

// TagFor builds the tag that ties a torrent to a request.
//
// This tag is the only reliable link between a torrent and the request that
// created it: the add call does not return a hash, and for hybrid v1/v2
// torrents the client's infohash can differ from the one the indexer
// advertised. Without it a retried activity cannot recognise its own work and
// would add a second copy.
func TagFor(requestID int64) string {
	return tagPrefix + strconv.FormatInt(requestID, 10)
}

// ErrNoSpace is the sentinel for "qBittorrent refused the torrent because
// there is not enough disk space to receive it". Two reasons it is a
// sentinel rather than a plain string match:
//
//   - The activity layer branches on it. A no-space failure is the one
//     failure that means "do not delete the half-added torrent and do not
//     blackhole the request in a loop": the operator has to free space
//     before anything else can move.
//   - The detection itself is heuristic and the check sits here once
//     rather than being redone in every caller.
var ErrNoSpace = errors.New("qbittorrent: not enough disk space")

// isFailedState reports whether a torrent's state means "the client refused
// or parked this torrent", as opposed to a state in which a real download
// is happening. The set is deliberately small: error, stalled, and
// missingfiles are the three the client uses when a torrent is on disk
// but is not making progress for an infrastructure reason.
//
// state strings are lowercased on the way through toStatus; this function
// expects the same form.
func isFailedState(state string) bool {
	switch state {
	case "error", "stalled", "missingfiles":
		return true
	}
	return false
}

// AddSpec describes a torrent to hand to the client.
type AddSpec struct {
	// Link is a magnet URI or a .torrent URL.
	Link string
	// Category files the torrent under a category in the client.
	Category string
	// Tags are applied to the torrent. Add always includes them, and looks
	// them up first.
	Tags []string
	// Paused adds the torrent without starting it.
	Paused bool
	// SavePath is the directory the client should download into. Empty means
	// the client's own default — Add sends no savepath parameter at all in
	// that case, so a deployment that does not need per-type routing is
	// unaffected.
	SavePath string
	// SequentialDownload maps to the qBittorrent "Download in sequential
	// order" add-option. Pieces are fetched in order from the start of
	// the file, so the file becomes playable while the rest is still
	// being grabbed.
	SequentialDownload bool
	// FirstLastPiecePrio maps to the qBittorrent "Download first and last
	// pieces first" add-option. The first and last pieces land first,
	// which lets a media player verify the file integrity (header +
	// footer) before the bulk arrives.
	FirstLastPiecePrio bool
}

// Add hands a torrent to the client and returns its hash.
//
// It is idempotent: if a torrent carrying one of the given tags is already
// present, that torrent is adopted and its hash returned with alreadyPresent
// true, and nothing is added. Temporal activities are retried after a worker
// crash, so this call will happen twice for the same release sooner or later.
//
// SavePath is sent only when set. An empty SavePath leaves the choice to
// the client's own default — that is the path a deployment that does not
// need per-type routing takes, and the field exists for the one that does.
func (c *Client) Add(ctx context.Context, spec AddSpec) (hash string, alreadyPresent bool, err error) {
	if spec.Link == "" {
		return "", false, errors.New("nothing to add: link is empty")
	}
	if len(spec.Tags) == 0 {
		// Refusing is better than adding an untagged torrent: an untagged
		// torrent can never be matched back to its request, so a retry would
		// duplicate it and cancellation could not clean it up.
		return "", false, errors.New("refusing to add a torrent with no tags")
	}

	for _, tag := range spec.Tags {
		existing, err := c.FindByTag(ctx, tag)
		if err != nil {
			return "", false, fmt.Errorf("look up torrents tagged %q: %w", tag, err)
		}
		if len(existing) > 0 {
			// A torrent the client already has is adopted, except when the
			// client has parked it in a failure state. That kind of
			// "already there" is a broken addition the caller has to
			// hear about, not a download that has started.
			if !isFailedState(existing[0].State) {
				c.log.Info("torrent already present, adopting it",
					"tag", tag, "hash", existing[0].Hash, "name", existing[0].Name)
				return existing[0].Hash, true, nil
			}
			c.log.Warn("torrent present in a failure state, retrying the add",
				"tag", tag, "hash", existing[0].Hash, "state", existing[0].State)
		}
	}

	options := map[string]string{
		"tags": strings.Join(spec.Tags, ","),
	}
	if spec.Category != "" {
		options["category"] = spec.Category
	}
	if spec.SavePath != "" {
		options["savepath"] = spec.SavePath
	}
	if spec.Paused {
		// The parameter was renamed in qBittorrent 5.0. Sending the wrong name
		// is not an error — the client ignores unknown fields — so the torrent
		// would silently start downloading.
		options[c.pauseOptionName()] = "true"
	}
	if spec.SequentialDownload {
		// "seqdl" has not been renamed across qBittorrent 4.x / 5.x the way
		// "paused" was, so the same option name works on every supported
		// version. Sending the field unconditionally is fine.
		options["seqdl"] = "true"
	}
	if spec.FirstLastPiecePrio {
		// Same — "flacp" is stable across versions.
		options["flacp"] = "true"
	}

	resp, err := c.qb.AddTorrentFromUrlCtx(ctx, spec.Link, options)
	if err != nil {
		return "", false, fmt.Errorf("add torrent to qbittorrent: %w", err)
	}

	// success_count and pending_count both mean "accepted"; failure_count is
	// the only one that means it did not work.
	if resp.FailureCount > 0 && resp.SuccessCount == 0 && resp.PendingCount == 0 {
		// qBittorrent's response itself is silent on the cause, but it may
		// have actually added the torrent and parked it in state=error with
		// a no-space errorMessage. Look it up; if that is the case, return
		// the sentinel so the activity layer can keep the torrent in place
		// and surface the problem to an operator rather than blackhole the
		// request.
		if noSpace, err := c.addFailedBecauseNoSpace(ctx, spec.Tags); err != nil {
			c.log.Warn("could not probe for a no-space condition", "error", err)
		} else if noSpace {
			return "", false, fmt.Errorf("torrent %s: %w", spec.Link, ErrNoSpace)
		}
		return "", false, fmt.Errorf("qbittorrent rejected the torrent (failures: %d)", resp.FailureCount)
	}

	// Newer clients report the hash straight away; older ones do not, so the
	// authoritative hash is read back from the client either way. Trusting the
	// indexer's infohash instead would be wrong for hybrid torrents.
	if len(resp.AddedTorrentIds) > 0 {
		return resp.AddedTorrentIds[0], false, nil
	}

	hash, err = c.waitForTag(ctx, spec.Tags)
	if err != nil {
		return "", false, err
	}
	return hash, false, nil
}

// waitForTag polls until a torrent carrying one of the tags appears.
func (c *Client) waitForTag(ctx context.Context, tags []string) (string, error) {
	deadline := time.Now().Add(hashDiscoveryTimeout)

	for {
		for _, tag := range tags {
			found, err := c.FindByTag(ctx, tag)
			if err != nil {
				return "", err
			}
			if len(found) > 0 {
				return found[0].Hash, nil
			}
		}

		if time.Now().After(deadline) {
			// The torrent was accepted but never appeared, which means it is
			// in the client somewhere this project cannot see it. Returning an
			// error leaves the request to be reconciled rather than pretending
			// it succeeded.
			return "", fmt.Errorf("torrent was accepted but did not appear in the client within %s",
				hashDiscoveryTimeout)
		}

		select {
		case <-ctx.Done():
			// Named explicitly rather than returning ctx.Err() alone: inside
			// an activity the bare "context deadline exceeded" says nothing
			// about what was being waited for, and this wait is the one place
			// where a timeout is an ordinary outcome rather than a bug.
			return "", fmt.Errorf("waiting for the torrent to appear in the client: %w", ctx.Err())
		case <-time.After(hashPollInterval):
		}
	}
}

// FindByTag returns every torrent carrying a tag.
func (c *Client) FindByTag(ctx context.Context, tag string) ([]domain.TorrentStatus, error) {
	torrents, err := c.qb.GetTorrentsCtx(ctx, qbittorrent.TorrentFilterOptions{Tag: tag})
	if err != nil {
		return nil, fmt.Errorf("list torrents by tag %q: %w", tag, err)
	}
	return toStatuses(torrents), nil
}

// Info returns the status of the named torrents, keyed by hash.
//
// Hashes that the client does not know are simply absent from the result:
// torrents do get removed by hand, and that is a state the caller has to
// handle rather than an error.
func (c *Client) Info(ctx context.Context, hashes []string) (map[string]domain.TorrentStatus, error) {
	if len(hashes) == 0 {
		return map[string]domain.TorrentStatus{}, nil
	}

	torrents, err := c.qb.GetTorrentsCtx(ctx, qbittorrent.TorrentFilterOptions{Hashes: hashes})
	if err != nil {
		return nil, fmt.Errorf("list torrents: %w", err)
	}

	out := make(map[string]domain.TorrentStatus, len(torrents))
	for _, s := range toStatuses(torrents) {
		out[s.Hash] = s
	}
	return out, nil
}

// AllActive returns every torrent that is still downloading.
//
// Used by the download watcher to poll everything in one request instead of
// one request per download.
func (c *Client) AllActive(ctx context.Context) ([]domain.TorrentStatus, error) {
	torrents, err := c.qb.GetTorrentsCtx(ctx, qbittorrent.TorrentFilterOptions{
		Filter: qbittorrent.TorrentFilterDownloading,
	})
	if err != nil {
		return nil, fmt.Errorf("list downloading torrents: %w", err)
	}
	return toStatuses(torrents), nil
}

// Delete removes a torrent, optionally with its files.
//
// deleteFiles must be chosen deliberately. It is safe when the client's save
// path is outside the Plex library, and destructive when it is inside: a
// downloaded file that Plex has already matched disappears from the library.
func (c *Client) Delete(ctx context.Context, hash string, deleteFiles bool) error {
	if hash == "" {
		return nil
	}
	if err := c.qb.DeleteTorrentsCtx(ctx, []string{hash}, deleteFiles); err != nil {
		return fmt.Errorf("delete torrent %s: %w", hash, err)
	}
	return nil
}

// pauseOptionName returns the add-option name this server version expects.
func (c *Client) pauseOptionName() string {
	if majorVersion(c.version) >= 5 {
		return "stopped"
	}
	// Also correct for an unreadable version: every release before 5.0 uses
	// "paused", and 5.0 still accepts it as an alias in practice.
	return "paused"
}

func majorVersion(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, ".-"); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func toStatuses(torrents []qbittorrent.Torrent) []domain.TorrentStatus {
	out := make([]domain.TorrentStatus, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, toStatus(t))
	}
	return out
}

func toStatus(t qbittorrent.Torrent) domain.TorrentStatus {
	s := domain.TorrentStatus{
		Hash:        t.Hash,
		Name:        t.Name,
		State:       strings.ToLower(string(t.State)),
		Progress:    t.Progress,
		DlSpeed:     t.DlSpeed,
		Size:        t.Size,
		SavePath:    t.SavePath,
		ContentPath: t.ContentPath,
		MaxProgress: t.Progress,
	}

	// The client reports ETA as a large sentinel when it cannot estimate, and
	// as -1 or 8640000 depending on version. Anything beyond a day is noise.
	if t.ETA > 0 && t.ETA < 86400 {
		s.ETA = t.ETA
	}

	if t.Tags != "" {
		for _, tag := range strings.Split(t.Tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				s.Tags = append(s.Tags, tag)
			}
		}
	}

	if t.AddedOn > 0 {
		s.AddedAt = time.Unix(t.AddedOn, 0)
	}

	return s
}

// truncate shortens a string for an error message.
//
// It exists because the string being truncated is whatever a misconfigured URL
// returned, which in practice is an entire HTML page.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// addFailedBecauseNoSpace reports whether a "torrent rejected" failure
// was actually a disk-full condition.
//
// qBittorrent returns failure_count > 0 for several different problems
// (bad magnet, missing metadata, denied by the disk), and the API does
// not say which. The check is by side effect: if the failure landed
// while free disk space was below the size of what was being added, the
// disk is the most likely cause.
//
// Two cases are explicitly NOT a no-space answer:
//   - The lookup returned an error. Treating "I do not know" as "yes,
//     disk is full" would route every silent failure through the
//     operator path.
//   - Free space is plenty. The cause has to be something else.
func (c *Client) addFailedBecauseNoSpace(ctx context.Context, tags []string) (bool, error) {
	free, err := c.qb.GetFreeSpaceOnDiskCtx(ctx)
	if err != nil {
		return false, err
	}
	// A negative answer from qBittorrent means the call itself failed,
	// not that the disk is full. Defensive: a non-negative but tiny
	// value still counts as "low", because qBittorrent's free-space
	// count lags the actual state of the filesystem and we want a
	// margin for whatever the operator has to write alongside.
	if free < 0 {
		return false, nil
	}
	for _, tag := range tags {
		found, err := c.FindByTag(ctx, tag)
		if err != nil {
			return false, err
		}
		for _, entry := range found {
			// qBittorrent's state strings, lowercased on the way through
			// toStatus. Anything other than the failure-related states
			// here means the torrent was accepted and is making
			// progress, so the rejection is on a different request.
			if entry.State == "error" || entry.State == "stalled" || entry.State == "missingfiles" {
				if entry.Size > 0 && free < entry.Size {
					c.log.Warn("qBittorrent rejected a torrent and free disk is below the size",
						"hash", entry.Hash, "size", entry.Size, "free", free, "state", entry.State)
					return true, nil
				}
			}
		}
	}
	return false, nil
}
