package request

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/qbit"
)

func TestAddTorrentWaitsPastThirtySecondsWithHeartbeats(t *testing.T) {
	var addedAt atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			fmt.Fprint(w, "Ok.")
		case "/api/v2/app/version":
			fmt.Fprint(w, "v4.6.4")
		case "/api/v2/torrents/add":
			addedAt.Store(time.Now().UnixNano())
			fmt.Fprint(w, "Ok.")
		case "/api/v2/torrents/info":
			w.Header().Set("Content-Type", "application/json")
			if added := addedAt.Load(); added != 0 && time.Since(time.Unix(0, added)) >= 31*time.Second {
				fmt.Fprint(w, `[{"hash":"abc123","tags":"mediadbot:21","state":"downloading"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.QBittorrent.URL = srv.URL
	log := slog.New(slog.DiscardHandler)
	client := qbit.New(cfg, log)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	acts := &Activities{qbit: client, cfg: cfg, log: log}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.SetTestTimeout(45 * time.Second)
	var heartbeats atomic.Int64
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
		heartbeats.Add(1)
	})
	env.RegisterActivity(acts.AddTorrent)
	value, err := env.ExecuteActivity(acts.AddTorrent, AddTorrentInput{
		RequestID: 21,
		Release:   domain.Release{DownloadURL: "http://prowlarr/download"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result AddTorrentResult
	if err := value.Get(&result); err != nil {
		t.Fatal(err)
	}
	if result.Hash != "abc123" || result.AlreadyPresent {
		t.Fatalf("unexpected result: %+v", result)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("no heartbeat while waiting for the torrent")
	}
}
