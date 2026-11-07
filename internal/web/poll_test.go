package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex/mediad/internal/storage"
)

// Run with MEDIAD_TEST_CHROME set to a Chrome executable; no live services needed.
func TestDashboardPollBrowser(t *testing.T) {
	chrome := os.Getenv("MEDIAD_TEST_CHROME")
	if chrome == "" {
		t.Skip("set MEDIAD_TEST_CHROME to run browser polling checks")
	}
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	var stage atomic.Int32
	slowStarted := make(chan struct{}, 1)
	slowRelease := make(chan struct{})
	now := time.Now()
	wall := func(n int) buckets {
		var result buckets
		for _, b := range dashboardBuckets {
			result.List = append(result.List, bucketView{dashboardBucket: b})
		}
		for i, state := range []storage.State{storage.StateDownloading, storage.StateSearching, storage.StateManual} {
			req := sampleRequest()
			req.ID += int64(i)
			req.State = state
			if i == 0 {
				switch n {
				case 1:
					req.Progress = 0.6
				case 2:
					req.State = storage.StateManual
				case 3:
					req.State = storage.StateAdding
				case 4:
					continue
				}
			}
			for j, b := range dashboardBuckets {
				for _, state := range b.States {
					if state == req.State {
						result.List[j].Rows = append(result.List[j].Rows, rowFor(req, nil, now, "/mediad"))
					}
				}
			}
		}
		return result
	}
	p := testPage()
	p.BasePath = "/mediad"
	filter := dashboardFilter{Scope: "mine", Q: "Игра", Sort: "waiting"}
	mux := http.NewServeMux()
	mux.Handle("/mediad/static/", http.StripPrefix("/mediad", s.staticHandler()))
	mux.HandleFunc("/stage", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil {
			http.Error(w, "invalid stage", 400)
			return
		}
		stage.Store(int32(n))
	})
	mux.HandleFunc("/slow-started", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-slowStarted:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/release-slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case slowRelease <- struct{}{}:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/mediad/fragments/active", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if stage.Load() == 6 {
			select {
			case slowStarted <- struct{}{}:
			case <-r.Context().Done():
				return
			}
			select {
			case <-slowRelease:
			case <-r.Context().Done():
				return
			}
		}
		if stage.Load() == 5 {
			http.Error(w, "fixture error", 500)
			return
		}
		s.renderFragment(w, r, "active_fragment", activeFragmentView{page: p, Filter: filter, buckets: wall(int(stage.Load()))})
	})
	mux.HandleFunc("/mediad/poster/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="92" height="138"></svg>`)
	})
	mux.HandleFunc("/mediad/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mediad/" {
			http.NotFound(w, r)
			return
		}
		s.render(w, r, "dashboard", dashboardView{page: p, Filter: filter, buckets: wall(0), ActiveAll: storage.ActiveStates()})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cmd := exec.CommandContext(t.Context(), "node", "testdata/poll.mjs", chrome, srv.URL, t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser polling: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}

func TestActiveFragmentPayload(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	for _, perBucket := range []int{0, 1, fragmentLimit} {
		wall := sampleBuckets(t)
		for i := range wall.List {
			row := wall.List[i].Rows[0]
			wall.List[i].Rows = nil
			for j := 0; j < perBucket; j++ {
				row.Href = fmt.Sprintf("/requests/%d", i*fragmentLimit+j)
				row.CancelHref = row.Href + "/cancel"
				wall.List[i].Rows = append(wall.List[i].Rows, row)
			}
		}
		rec := httptest.NewRecorder()
		s.renderFragment(rec, httptest.NewRequest("GET", "/fragments/active", nil), "active_fragment", activeFragmentView{page: testPage(), buckets: wall})
		if rec.Code != 200 {
			t.Fatalf("render: %d", rec.Code)
		}
		// A fragment must remain a bounded collection of rows, not a whole page.
		if got := strings.Count(rec.Body.String(), `data-request=`); got != 3*perBucket {
			t.Fatalf("rows: %d, want %d", got, 3*perBucket)
		}
		t.Logf("%d rows: %d uncompressed bytes", 3*perBucket, rec.Body.Len())
	}
}
