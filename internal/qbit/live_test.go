package qbit

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alex/mediad/internal/config"
)

// These tests run against a real qBittorrent instance and are skipped unless
// one is configured. They exist because the unit tests all run against a stub
// that this project wrote, and a stub cannot disagree with you: every field
// name, every state string and every version difference it encodes is an
// assumption, not evidence.
//
// Running them requires a live server and, on a real library, is read-only.
// Nothing here adds, deletes, pauses or resumes anything.
//
//	MEDIAD_TEST_QBIT_URL=http://host:8112 \
//	MEDIAD_TEST_QBIT_API_KEY=qbt_... \
//	go test ./internal/qbit/ -run TestLive -v
const (
	liveURLEnv = "MEDIAD_TEST_QBIT_URL"
	liveKeyEnv = "MEDIAD_TEST_QBIT_API_KEY"
)

func liveClient(t *testing.T) *Client {
	t.Helper()

	url := os.Getenv(liveURLEnv)
	if url == "" {
		t.Skipf("%s is not set; point it at a qBittorrent Web API to run the live tests", liveURLEnv)
	}

	cfg := &config.Config{}
	cfg.QBittorrent.URL = url
	cfg.QBittorrent.APIKey = os.Getenv(liveKeyEnv)
	cfg.QBittorrent.Username = os.Getenv("MEDIAD_TEST_QBIT_USERNAME")
	cfg.QBittorrent.Password = os.Getenv("MEDIAD_TEST_QBIT_PASSWORD")
	cfg.QBittorrent.PollInterval = 30 * time.Second

	c := New(cfg, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start against %s: %v", url, err)
	}
	return c
}

// TestLiveVersionIsRecognised is the check that the version gate does not
// reject a real client, and that the pause option name follows from it.
func TestLiveVersionIsRecognised(t *testing.T) {
	c := liveClient(t)

	v := c.Version()
	t.Logf("connected to qBittorrent %s", v)

	if v == "" {
		t.Fatal("the version was not read from a real client")
	}
	if !looksLikeVersion(v) {
		t.Fatalf("a real qBittorrent reported version %q, which our own check rejects", v)
	}

	// 5.x renamed the parameter; getting this wrong is silent, because the
	// client ignores unknown fields and simply starts the torrent.
	want := "paused"
	if major := majorVersion(v); major >= 5 {
		want = "stopped"
	}
	if got := c.pauseOptionName(); got != want {
		t.Errorf("version %s: pause option = %q, want %q", v, got, want)
	}
	t.Logf("pause option for %s is %q", v, c.pauseOptionName())
}

// TestLiveTorrentListDecodes reads the whole library and checks that every
// torrent decodes and that the state strings are ones the domain layer
// understands.
//
// A state the domain layer does not recognise is not an error — Done(),
// Failed() and Stalled() all return false for it and the download loop simply
// keeps waiting — so an unrecognised state is invisible in production and is
// exactly what this test is for.
func TestLiveTorrentListDecodes(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	all, err := c.FindByTag(ctx, "")
	if err != nil {
		t.Fatalf("listing every torrent: %v", err)
	}
	t.Logf("the library holds %d torrents", len(all))

	if len(all) == 0 {
		t.Skip("the library is empty; nothing to decode")
	}

	var (
		recognised, unrecognised int
		seenStates               = map[string]int{}
	)
	for _, s := range all {
		seenStates[s.State]++

		if s.Hash == "" {
			t.Errorf("a torrent decoded with an empty hash: %+v", s)
		}

		switch {
		case s.Done(), s.Failed(), s.Stalled():
			recognised++
		case s.State == "downloading", s.State == "stopeddl", s.State == "stoppeddl",
			s.State == "queueddl", s.State == "checkingdl", s.State == "metadl",
			s.State == "checkingresumedata", s.State == "allocating",
			s.State == "moving", s.State == "forceddl", s.State == "forcedmetadl":
			recognised++
		default:
			unrecognised++
			t.Errorf("state %q is not one this project recognises; "+
				"Done/Failed/Stalled all return false for it, so a download in it would be waited on forever",
				s.State)
		}
	}

	t.Logf("states seen: %v", seenStates)
	t.Logf("recognised %d of %d", recognised, recognised+unrecognised)

	// The completion states are the ones that end a request, so at least one
	// of them appearing in a real library is worth confirming by name.
	for _, expected := range []string{"uploading", "stalledup", "stoppedup", "forcedup", "queuedup", "checkingup"} {
		if n := seenStates[expected]; n > 0 {
			t.Logf("completion state %q seen %d times and is treated as done", expected, n)
		}
	}
}

// TestLiveInfoByHash exercises the batched lookup the download watcher uses.
func TestLiveInfoByHash(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	all, err := c.FindByTag(ctx, "")
	if err != nil {
		t.Fatalf("listing torrents: %v", err)
	}
	if len(all) == 0 {
		t.Skip("the library is empty")
	}

	// A handful of real hashes, plus one that cannot exist.
	hashes := []string{"0000000000000000000000000000000000000000"}
	for i, s := range all {
		if i >= 3 {
			break
		}
		hashes = append(hashes, s.Hash)
	}

	got, err := c.Info(ctx, hashes)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}

	// The batch returned real rows, and the invented hash is simply absent —
	// that is the contract the download loop relies on when a torrent has been
	// removed by hand.
	if len(got) == 0 {
		t.Fatal("a batched lookup of real hashes returned nothing")
	}
	if _, ok := got["0000000000000000000000000000000000000000"]; ok {
		t.Error("an impossible hash was reported as present")
	}
	t.Logf("batched lookup returned %d of %d requested", len(got), len(hashes))

	for h, s := range got {
		if s.State == "" {
			t.Errorf("hash %s decoded with an empty state", h)
		}
		// A real client reports the sentinel 8640000 when it cannot estimate,
		// and a value that large must not reach the interface as a duration.
		if s.ETA > 86400 {
			t.Errorf("hash %s: ETA %d seconds reached the domain layer; "+
				"the sentinel should have been dropped", h, s.ETA)
		}
	}
}

// TestLiveAllActive exercises the filter the download watcher polls with.
//
// It is worth checking against a real server because qBittorrent renamed some
// filter values in 5.0, and an unknown filter is not rejected — it quietly
// returns the wrong set.
func TestLiveAllActive(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	active, err := c.AllActive(ctx)
	if err != nil {
		t.Fatalf("AllActive: %v", err)
	}
	t.Logf("the filter returned %d downloading torrents", len(active))

	for _, s := range active {
		if s.Hash == "" {
			t.Error("a torrent decoded with an empty hash")
		}
		// The filter is meant to select downloads, so anything fully
		// downloaded coming back means the filter value is not doing what we
		// think it is.
		if s.Progress >= 1.0 {
			t.Errorf("a fully downloaded torrent (%s) came back from the downloading filter", s.State)
		}
	}
}

// TestLiveAbsentTagReturnsEmpty is the read path the idempotent add depends
// on: the tag lookup has to come back empty rather than erroring, or Add can
// never tell whether a torrent is already present.
func TestLiveAbsentTagReturnsEmpty(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	found, err := c.FindByTag(ctx, "mediadbot:this-tag-does-not-exist-999999")
	if err != nil {
		t.Fatalf("a tag that matches nothing must not be an error: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("an invented tag matched %d torrents", len(found))
	}
}
