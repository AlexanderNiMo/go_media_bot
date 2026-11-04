package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alex/mediad/internal/storage"
	"golang.org/x/net/html"
)

func TestDashboardHasFilters(t *testing.T) {
	for _, tc := range []struct {
		filter dashboardFilter
		want   bool
	}{
		{dashboardFilter{}, false},
		{dashboardFilter{Scope: "mine", Sort: "title"}, false},
		{dashboardFilter{Q: "фильм"}, true},
		{dashboardFilter{Type: "tv"}, true},
		{dashboardFilter{Since: "7d"}, true},
		{dashboardFilter{ActiveSubs: []string{"manual"}}, true},
	} {
		if got := tc.filter.HasFilters(); got != tc.want {
			t.Errorf("%+v: got %v, want %v", tc.filter, got, tc.want)
		}
	}
}

func TestEmptyPagesOfferNextStep(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	p := testPage()
	p.BasePath = "/mediad"
	var empty buckets
	for _, b := range dashboardBuckets {
		empty.List = append(empty.List, bucketView{dashboardBucket: b})
	}
	req := sampleRequest()
	req.State = storage.StateManual
	detail := detailFor(req, nil, nil, 0, req.CreatedAt, p.BasePath)
	for _, tc := range []struct {
		name, template string
		data           any
		text, href     string
	}{
		{"dashboard mine", "dashboard", dashboardView{page: p, Filter: dashboardFilter{Scope: "mine"}, buckets: empty}, "В этом списке пока нет завершённых запросов.", "/mediad/search"},
		{"dashboard filtered mine", "dashboard", dashboardView{page: p, Filter: dashboardFilter{Scope: "mine", Q: "нет"}, buckets: empty}, "Под текущие фильтры", "/mediad/?scope=mine"},
		{"dashboard filtered admin", "dashboard", dashboardView{page: p, Filter: dashboardFilter{Scope: "all", Since: "7d"}, buckets: empty}, "Сбросить фильтры", "/mediad/?scope=all"},
		{"requests active", "requests", requestsView{page: p}, "Нет запросов в выбранных состояниях.", "/mediad/requests?state=all"},
		{"requests filtered", "requests", requestsView{page: p, State: "failed"}, "Нет запросов в выбранных состояниях.", "/mediad/requests?state=all"},
		{"requests all", "requests", requestsView{page: p, State: "all"}, "Запросов пока нет.", "/mediad/search"},
		{"search", "search", searchView{page: p, Query: "<нет>"}, "Попробуйте оригинальное название", "/mediad/search"},
		{"notifications", "notifications", notificationsView{page: p}, "Нет непрочитанных уведомлений.", ""},
		{"seasons", "seasons", seasonsView{page: p}, "В TMDB пока нет сезонов", "/mediad/search"},
		{"manual", "request", requestView{page: p, Request: detail}, "Список релизов для выбора пуст.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest("GET", "/", nil), tc.template, tc.data)
			body := rec.Body.String()
			if rec.Code != 200 {
				t.Fatalf("render: %d: %s", rec.Code, body)
			}
			if !strings.Contains(body, tc.text) {
				t.Errorf("missing guidance %q", tc.text)
			}
			if strings.Contains(body, "<нет>") {
				t.Error("search query was not escaped")
			}
			if tc.href != "" && !strings.Contains(body, `href="`+tc.href+`"`) && !strings.Contains(body, `action="`+tc.href+`"`) {
				t.Errorf("missing next step %s", tc.href)
			}
			if tc.name == "dashboard mine" && strings.Contains(body, "Сбросить фильтры") {
				t.Error("scope alone must not offer a filter reset")
			}
			if tc.template == "dashboard" && !strings.Contains(body, "ожидающих выбора релиза или новых серий") {
				t.Error("empty waiting bucket must account for ongoing seasons")
			}
			doc, err := html.Parse(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			checkControlsHaveNames(t, doc)
			checkInputsHaveLabels(t, doc, labelTargets(doc))
			checkNothingFocusableIsHidden(t, doc)
		})
	}
}

func TestManualEmptyGuidanceOnlyWithoutCandidates(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	populated := fixtures(t)["request"].(requestView)
	for _, tc := range []struct {
		state      storage.State
		candidates bool
		want       bool
	}{
		{storage.StateManual, false, true},
		{storage.StateManual, true, false},
		{storage.StateOngoing, false, false},
		{storage.StateDownloading, false, false},
	} {
		v := populated
		v.Request.State = tc.state
		if !tc.candidates {
			v.Candidates = nil
		}
		rec := httptest.NewRecorder()
		s.render(rec, httptest.NewRequest("GET", "/", nil), "request", v)
		if rec.Code != 200 {
			t.Fatalf("render: %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Это не возобновляет поиск.") {
			t.Error("empty submission must not promise to resume searching")
		}
		if got := strings.Contains(rec.Body.String(), "Список релизов для выбора пуст."); got != tc.want {
			t.Errorf("state=%s candidates=%v: empty guidance=%v, want %v", tc.state, tc.candidates, got, tc.want)
		}
	}
}
