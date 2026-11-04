package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestRemainingTablesCarryResponsiveLabels(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	data := fixtures(t)
	settings := data["settings"].(settingsView)
	settings.Telegram = []telegramRow{{ID: "123", CreatedAt: "2026-11-04"}}
	data["settings"] = settings
	for _, name := range []string{"request", "requests", "notifications", "settings"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data[name])
			doc, err := html.Parse(strings.NewReader(rec.Body.String()))
			if err != nil {
				t.Fatal(err)
			}
			tables := 0
			var walk func(*html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "table" {
					tables++
					if attrForTest(n, "role") != "table" || !strings.Contains(attrForTest(n, "class"), "table-cards") {
						t.Error("table needs responsive cards and explicit semantics")
					}
					checkRowRolesAndLabels(t, n)
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(doc)
			want := map[string]int{"request": 2, "requests": 1, "notifications": 1, "settings": 3}[name]
			if tables != want {
				t.Fatalf("got %d tables, want %d", tables, want)
			}
		})
	}
}

func TestResponsivePostersKeepDeploymentPrefix(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	data := fixtures(t)
	search := data["search"].(searchView)
	search.BasePath = "/mediad"
	data["search"] = search
	confirm := data["confirm"].(confirmView)
	confirm.BasePath = "/mediad"
	confirm.PosterURL = "/poster/w500/p.jpg"
	data["confirm"] = confirm
	seasons := data["seasons"].(seasonsView)
	seasons.BasePath = "/mediad"
	seasons.Show.PosterURL = "/poster/w342/p.jpg"
	data["seasons"] = seasons
	for _, name := range []string{"request", "cancel"} {
		req := sampleRequest()
		detail := detailFor(req, nil, nil, 0, req.CreatedAt, "/mediad")
		detail.PosterURL = "/mediad/poster/w185/p.jpg"
		if name == "request" {
			data[name] = requestView{page: testPage(), Request: detail}
		} else {
			data[name] = cancelView{page: testPage(), Request: detail}
		}
	}
	for _, name := range []string{"search", "confirm", "seasons", "request", "cancel"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data[name])
			body := rec.Body.String()
			size := map[string]string{"search": "w342", "confirm": "w500", "seasons": "w342", "request": "w185", "cancel": "w185"}[name]
			if !strings.Contains(body, `src="/mediad/poster/`+size+`/p.jpg"`) {
				t.Error("desktop fallback lost its size or deployment prefix")
			}
			if !strings.Contains(body, `media="(max-width: 767px)" srcset="/mediad/poster/w185/p.jpg"`) {
				t.Error("mobile poster lost the deployment prefix or small source")
			}
		})
	}
}

func TestMobilePosterPreservesEscapedPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/mediad/poster/w500/ab/a%20b.jpg", "/mediad/poster/w185/ab/a%20b.jpg"},
		{"/poster/w342/p.jpg", "/poster/w185/p.jpg"},
		{"", ""}, {"/poster/w342", "/poster/w342"}, {"/other.jpg", "/other.jpg"},
	} {
		if got := mobilePoster(tc.in); got != tc.want {
			t.Errorf("mobilePoster(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
