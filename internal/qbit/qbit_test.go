package qbit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// fakeQBittorrent is a stub of the parts of the Web API this project uses.
type fakeQBittorrent struct {
	mu sync.Mutex

	version string
	// torrents is what /torrents/info returns, keyed by hash.
	torrents map[string]map[string]any

	// addCalls records the option maps passed to /torrents/add, so a test can
	// assert both what was sent and whether it was sent at all.
	addCalls []url.Values
	// deleteCalls records (hash, deleteFiles) pairs.
	deleteCalls []string

	// addedIDs is returned in the add response. Empty means "this client
	// version does not report hashes", which is the case that forces the
	// tag-polling path.
	addedIDs []string
	// addResponse overrides the add response body when set.
	addResponse string
	// addResponseJSON makes addResponse be served as application/json, which
	// is what selects the client library's structured-response path.
	addResponseJSON bool
	// loginStatus lets a test simulate a rejected login.
	loginStatus int
}

func newFakeQBittorrent() *fakeQBittorrent {
	return &fakeQBittorrent{
		version:  "v4.6.4",
		torrents: map[string]map[string]any{},
	}
}

func (f *fakeQBittorrent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/api/v2/auth/login":
		if f.loginStatus != 0 {
			w.WriteHeader(f.loginStatus)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test-session", Path: "/"})
		_, _ = w.Write([]byte("Ok."))

	case r.URL.Path == "/api/v2/app/version":
		_, _ = w.Write([]byte(f.version))

	case r.URL.Path == "/api/v2/torrents/info":
		_ = r.ParseForm()
		hashes := map[string]bool{}
		for _, h := range strings.Split(r.Form.Get("hashes"), "|") {
			if h != "" {
				hashes[h] = true
			}
		}
		tag := r.Form.Get("tag")

		out := []map[string]any{}
		for hash, t := range f.torrents {
			if len(hashes) > 0 && !hashes[hash] {
				continue
			}
			if tag != "" && !hasTag(t, tag) {
				continue
			}
			out = append(out, t)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)

	case r.URL.Path == "/api/v2/torrents/add":
		_ = r.ParseForm()
		f.addCalls = append(f.addCalls, r.Form)

		if f.addResponse != "" {
			if f.addResponseJSON {
				w.Header().Set("Content-Type", "application/json")
			}
			_, _ = w.Write([]byte(f.addResponse))
			return
		}
		if len(f.addedIDs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success_count":     1,
				"added_torrent_ids": f.addedIDs,
			})
			return
		}
		_, _ = w.Write([]byte("Ok."))

	case r.URL.Path == "/api/v2/torrents/delete":
		_ = r.ParseForm()
		f.deleteCalls = append(f.deleteCalls, r.Form.Get("hashes")+"/"+r.Form.Get("deleteFiles"))
		for _, h := range strings.Split(r.Form.Get("hashes"), "|") {
			delete(f.torrents, h)
		}
		_, _ = w.Write([]byte("Ok."))

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func hasTag(t map[string]any, tag string) bool {
	raw, _ := t["tags"].(string)
	for _, got := range strings.Split(raw, ",") {
		if strings.TrimSpace(got) == tag {
			return true
		}
	}
	return false
}

func newTestClient(t *testing.T, fake *fakeQBittorrent) *Client {
	t.Helper()

	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.QBittorrent.URL = srv.URL
	cfg.QBittorrent.Username = "admin"
	cfg.QBittorrent.Password = "secret"
	cfg.QBittorrent.PollInterval = 30 * time.Second

	c := New(cfg, slog.New(slog.DiscardHandler))
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return c
}

func torrent(hash, name, state string, progress float64, tags string) map[string]any {
	return map[string]any{
		"hash":      hash,
		"name":      name,
		"state":     state,
		"progress":  progress,
		"tags":      tags,
		"size":      1000,
		"dlspeed":   100,
		"eta":       60,
		"save_path": "/downloads",
	}
}

// TestAddIsIdempotentByTag is the load-bearing behaviour of this package.
// Temporal retries activities after a worker crash, so Add will be called
// twice for the same release sooner or later, and a second copy of a 15 GB
// torrent is not an acceptable outcome.
func TestAddIsIdempotentByTag(t *testing.T) {
	fake := newFakeQBittorrent()
	tag := TagFor(42)
	fake.torrents["abc123"] = torrent("abc123", "Матрица", "downloading", 0.1, tag)

	c := newTestClient(t, fake)

	hash, already, err := c.Add(context.Background(), AddSpec{
		Link: "magnet:?xt=urn:btih:whatever",
		Tags: []string{tag},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if !already {
		t.Error("alreadyPresent = false, want true: the tag was already in the client")
	}
	if hash != "abc123" {
		t.Errorf("hash = %q, want the existing torrent's", hash)
	}
	if len(fake.addCalls) != 0 {
		t.Errorf("a second copy was added: %d add calls", len(fake.addCalls))
	}
}

// TestAddRefusesWithoutTags — an untagged torrent can never be matched back to
// its request, so a retry would duplicate it and cancellation could not clean
// it up.
func TestAddRefusesWithoutTags(t *testing.T) {
	fake := newFakeQBittorrent()
	c := newTestClient(t, fake)

	if _, _, err := c.Add(context.Background(), AddSpec{Link: "magnet:?xt=1"}); err == nil {
		t.Fatal("expected an error when adding without tags")
	}
	if len(fake.addCalls) != 0 {
		t.Error("an untagged torrent reached the client")
	}
}

func TestAddLearnsHashFromClientResponse(t *testing.T) {
	fake := newFakeQBittorrent()
	// qBittorrent 5.x reports the hash straight away.
	fake.addedIDs = []string{"deadbeef"}

	c := newTestClient(t, fake)

	hash, already, err := c.Add(context.Background(), AddSpec{
		Link: "magnet:?xt=1",
		Tags: []string{TagFor(7)},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if already {
		t.Error("alreadyPresent = true for a fresh add")
	}
	if hash != "deadbeef" {
		t.Errorf("hash = %q, want the one the client reported", hash)
	}

	if len(fake.addCalls) != 1 {
		t.Fatalf("add calls = %d, want 1", len(fake.addCalls))
	}
	// The tag must actually be sent, or the torrent cannot be found again.
	if got := fake.addCalls[0].Get("tags"); got != TagFor(7) {
		t.Errorf("tags sent = %q, want %q", got, TagFor(7))
	}
	// No save path: the client owns where downloads go.
	if got := fake.addCalls[0].Get("savepath"); got != "" {
		t.Errorf("savepath = %q, want it left to the client", got)
	}
}

// TestAddPollsForHashOnOlderClients covers the case the tag exists for: a
// client that returns no hash, so the authoritative one has to be read back.
func TestAddPollsForHashOnOlderClients(t *testing.T) {
	fake := newFakeQBittorrent()
	tag := TagFor(9)
	// The torrent appears in the client only after the add, as it would in
	// reality — the add call is asynchronous.
	fake.torrents["feedface"] = torrent("feedface", "Release", "downloading", 0.0, tag)

	c := newTestClient(t, fake)

	hash, _, err := c.Add(context.Background(), AddSpec{Link: "magnet:?xt=1", Tags: []string{tag}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hash != "feedface" {
		t.Errorf("hash = %q, want the one read back from the client", hash)
	}
}

func TestAddErrorsWhenTorrentNeverAppears(t *testing.T) {
	fake := newFakeQBittorrent()
	c := newTestClient(t, fake)

	// hashDiscoveryTimeout is 30s, which is far too long for a test; the
	// context deadline is what ends the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := c.Add(ctx, AddSpec{Link: "magnet:?xt=1", Tags: []string{TagFor(11)}})
	if err == nil {
		t.Fatal("expected an error when the torrent is accepted but never appears")
	}
}

func TestAddReportsClientRejection(t *testing.T) {
	fake := newFakeQBittorrent()
	fake.addResponse = `{"success_count":0,"failure_count":1,"pending_count":0}`
	fake.addResponseJSON = true

	c := newTestClient(t, fake)

	_, _, err := c.Add(context.Background(), AddSpec{Link: "magnet:?xt=1", Tags: []string{TagFor(12)}})
	if err == nil {
		t.Fatal("expected an error when the client reports a failure")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want it to say the torrent was rejected", err)
	}
}

// TestAddConfirmsThroughTheTagNotTheResponse documents why the tag poll is not
// redundant.
//
// qBittorrent answers a plain 200 with the body "Ok." for an accepted torrent
// and "Fails." for a rejected one — and the client library maps both to
// SuccessCount: 1, because it does not look at the body. So the HTTP response
// cannot be trusted to say whether anything was actually added, and the tag
// lookup is the only real confirmation.
func TestAddConfirmsThroughTheTagNotTheResponse(t *testing.T) {
	fake := newFakeQBittorrent()
	// A 200 with a text/plain body: indistinguishable from success to the
	// library, and nothing appears in the client.
	fake.addResponse = "Fails."

	c := newTestClient(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, _, err := c.Add(ctx, AddSpec{Link: "magnet:?xt=1", Tags: []string{TagFor(13)}})
	if err == nil {
		t.Fatal("expected the tag lookup to catch a torrent that was never added")
	}
	// Either deadline can fire first — the context's or the discovery
	// timeout's — and both have to say what was being waited for, because
	// inside an activity a bare "context deadline exceeded" is unactionable.
	if !strings.Contains(err.Error(), "appear in the client") {
		t.Errorf("error = %v, want it to report that the torrent never appeared", err)
	}
}

// TestPauseOptionNameTracksClientVersion — qBittorrent 5.0 renamed the
// parameter. The library passes options through verbatim and the client
// ignores unknown fields, so sending the wrong name is not an error: the
// torrent simply starts downloading when it was meant to wait.
func TestPauseOptionNameTracksClientVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"v4.6.4", "paused"},
		{"4.6.4", "paused"},
		{"v5.0.3", "stopped"},
		{"5.2.4", "stopped"},
		{"", "paused"}, // unreadable version falls back to the older name
		{"garbage", "paused"},
	} {
		fake := newFakeQBittorrent()
		fake.version = tc.version
		c := newTestClient(t, fake)

		if got := c.pauseOptionName(); got != tc.want {
			t.Errorf("version %q: pauseOptionName = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestAddSendsVersionAppropriatePauseFlag(t *testing.T) {
	fake := newFakeQBittorrent()
	fake.version = "v5.0.3"
	fake.addedIDs = []string{"h1"}
	c := newTestClient(t, fake)

	if _, _, err := c.Add(context.Background(), AddSpec{
		Link: "magnet:?xt=1", Tags: []string{TagFor(1)}, Paused: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	sent := fake.addCalls[0]
	if sent.Get("stopped") != "true" {
		t.Errorf("stopped = %q, want true on a 5.x client", sent.Get("stopped"))
	}
	if sent.Get("paused") != "" {
		t.Errorf("paused = %q, want it unused on a 5.x client", sent.Get("paused"))
	}
}

func TestFindByTag(t *testing.T) {
	fake := newFakeQBittorrent()
	tag := TagFor(5)
	fake.torrents["a"] = torrent("a", "one", "downloading", 0.5, tag)
	fake.torrents["b"] = torrent("b", "two", "downloading", 0.5, "other")
	fake.torrents["c"] = torrent("c", "three", "downloading", 0.5, tag+",other")

	c := newTestClient(t, fake)

	found, err := c.FindByTag(context.Background(), tag)
	if err != nil {
		t.Fatalf("FindByTag: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d torrents, want 2", len(found))
	}
}

func TestInfoOmitsUnknownHashes(t *testing.T) {
	fake := newFakeQBittorrent()
	fake.torrents["known"] = torrent("known", "x", "downloading", 0.25, "")

	c := newTestClient(t, fake)

	got, err := c.Info(context.Background(), []string{"known", "vanished"})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}

	// A torrent removed by hand must be absent from the result, not an error:
	// the caller has to notice and react, and that is a state, not a failure.
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	if _, ok := got["known"]; !ok {
		t.Error("the known hash is missing")
	}
	if _, ok := got["vanished"]; ok {
		t.Error("an unknown hash was reported as present")
	}
}

func TestDelete(t *testing.T) {
	fake := newFakeQBittorrent()
	fake.torrents["h"] = torrent("h", "x", "downloading", 0.1, "")
	c := newTestClient(t, fake)

	if err := c.Delete(context.Background(), "h", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fake.deleteCalls) != 1 {
		t.Fatalf("delete calls = %d", len(fake.deleteCalls))
	}
	if fake.deleteCalls[0] != "h/true" {
		t.Errorf("delete called with %q, want h/true", fake.deleteCalls[0])
	}

	// An empty hash is a no-op rather than an error: callers compensate
	// best-effort and may not have a hash to work with.
	if err := c.Delete(context.Background(), "", true); err != nil {
		t.Errorf("deleting an empty hash: %v", err)
	}
}

func TestLoginFailureIsReported(t *testing.T) {
	fake := newFakeQBittorrent()
	// A 403 is what qBittorrent returns for a banned IP, not just a bad
	// password, which is why the error message mentions it.
	fake.loginStatus = http.StatusForbidden

	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.QBittorrent.URL = srv.URL
	cfg.QBittorrent.Username = "admin"
	cfg.QBittorrent.Password = "wrong"

	c := New(cfg, slog.New(slog.DiscardHandler))
	err := c.Start(context.Background())
	if err == nil {
		t.Fatal("expected a login failure")
	}
	if !strings.Contains(err.Error(), "banned") {
		t.Errorf("error = %v, want it to mention the IP ban, which reads like a wrong password", err)
	}
}

// ---------------------------------------------------------------------------
// Status normalisation
// ---------------------------------------------------------------------------

// TestStatusNormalisation covers the qBittorrent 5.x rename and the states
// that mean "stop waiting". These strings are the only place the two versions
// differ, and getting them wrong means a finished download is never noticed.
func TestStatusNormalisation(t *testing.T) {
	for _, tc := range []struct {
		state   string
		done    bool
		failed  bool
		stalled bool
	}{
		{"downloading", false, false, false},
		{"stalledDL", false, false, true},
		{"metaDL", false, false, true},
		{"error", false, true, false},
		{"missingFiles", false, true, false},
		{"uploading", true, false, false},
		{"stalledUP", true, false, false},
		// 4.x naming
		{"pausedUP", true, false, false},
		// 5.x naming: the rename that breaks naive state switches
		{"stoppedUP", true, false, false},
		{"stoppedDL", false, false, false},
	} {
		fake := newFakeQBittorrent()
		fake.torrents["h"] = torrent("h", "x", tc.state, 0.5, "")
		c := newTestClient(t, fake)

		got, err := c.Info(context.Background(), []string{"h"})
		if err != nil {
			t.Fatalf("state %q: %v", tc.state, err)
		}
		s := got["h"]

		// The state is lower-cased by the client wrapper so callers do not
		// have to know the original casing.
		if s.State != strings.ToLower(tc.state) {
			t.Errorf("state %q: normalised to %q", tc.state, s.State)
		}
		if s.Done() != tc.done {
			t.Errorf("state %q: Done = %v, want %v", tc.state, s.Done(), tc.done)
		}
		if s.Failed() != tc.failed {
			t.Errorf("state %q: Failed = %v, want %v", tc.state, s.Failed(), tc.failed)
		}
		if s.Stalled() != tc.stalled {
			t.Errorf("state %q: Stalled = %v, want %v", tc.state, s.Stalled(), tc.stalled)
		}
	}
}

func TestProgressOneIsDoneRegardlessOfState(t *testing.T) {
	fake := newFakeQBittorrent()
	// A torrent that reached 100% is done even if the state string is one we
	// do not recognise, which happens across client versions.
	fake.torrents["h"] = torrent("h", "x", "someUnknownState", 1.0, "")
	c := newTestClient(t, fake)

	got, err := c.Info(context.Background(), []string{"h"})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !got["h"].Done() {
		t.Error("a torrent at 100% must count as done")
	}
	if !got["h"].Downloaded() {
		t.Error("a torrent at 100% with no error must count as downloaded")
	}
}

func TestETASentinelIsDropped(t *testing.T) {
	fake := newFakeQBittorrent()
	tor := torrent("h", "x", "downloading", 0.5, "")
	tor["eta"] = 8640000 // the client's "unknown" sentinel
	fake.torrents["h"] = tor
	c := newTestClient(t, fake)

	got, err := c.Info(context.Background(), []string{"h"})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got["h"].ETA != 0 {
		t.Errorf("ETA = %d, want 0 for an unknown estimate", got["h"].ETA)
	}
}

func TestTagFor(t *testing.T) {
	if got := TagFor(42); got != "mediadbot:42" {
		t.Errorf("TagFor(42) = %q", got)
	}
	// The prefix keeps this project's tags distinguishable from a user's own.
	if !strings.HasPrefix(TagFor(1), "mediadbot:") {
		t.Error("tags must be namespaced")
	}
}

func TestDownloadedExcludesFailed(t *testing.T) {
	failed := domain.TorrentStatus{Progress: 1.0, State: "error"}
	if failed.Downloaded() {
		t.Error("a torrent at 100% but in an error state must not count as downloaded")
	}
}
