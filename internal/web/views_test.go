package web

import (
	"strings"
	"testing"
	"time"

	"github.com/alex/mediad/internal/storage"
)

// The status line.
//
// One sentence per state, and every number in it comes from the request
// itself: a sentence that could be wrong is worse than no sentence at all.
// The tests are table-driven, because the failure mode is a wrong number in
// a sentence that reads perfectly well.

func TestStatusLine(t *testing.T) {
	now := time.Now()

	// A request that has been looking for four months and is due to look
	// again in six hours.
	base := func() *storage.Request {
		r := sampleRequest()
		r.CreatedAt = now.Add(-120 * 24 * time.Hour)
		r.AttemptCount = 47
		r.NextRunAt = &now
		r.NextRunAt = ptrTime(now.Add(6 * time.Hour))
		return r
	}

	for _, tc := range []struct {
		name       string
		request    func() *storage.Request
		candidates int
		want       string
	}{
		{
			name: "searching",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateSearching
				return r
			},
			want: "Ищет релиз с русским звуком. Проверено 47 раз за 4 месяца. " +
				"Следующая проверка через 6 часов.",
		},
		{
			name: "pending, never searched",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StatePending
				r.AttemptCount = 0
				return r
			},
			want: "Ждёт первой проверки.",
		},
		{
			name: "pending, searched a few times",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StatePending
				r.AttemptCount = 5
				return r
			},
			want: "Проверено 5 раз. Следующая проверка через 6 часов.",
		},
		{
			name: "downloading",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateDownloading
				return r
			},
			want: "Качает 9,1 ГиБ из 21,0 ГиБ (43%).",
		},
		{
			name: "manual, with candidates",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateManual
				return r
			},
			candidates: 3,
			want:       "Индексатор вернул 3 кандидата, авто-подбор не выбрал ни один. Нужен ваш выбор.",
		},
		{
			name: "manual, nothing came back",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateManual
				return r
			},
			want: "Индексатор ничего не вернул. Релиз можно прикрепить вручную.",
		},
		{
			name: "ongoing",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateOngoing
				r.SelectedVideoCount = 5
				r.SelectedEpisodeCount = 8
				return r
			},
			want: "Ждёт остальные серии: 5 из 8. Сезон ещё выходит.",
		},
		{
			name: "completed",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateCompleted
				return r
			},
			want: "Загрузка завершена.",
		},
		{
			name: "failed",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateFailed
				r.ErrorStreak = 3
				return r
			},
			want: "Поиск остановлен после 3 ошибок подряд.",
		},
		{
			name: "canceled",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateCanceled
				return r
			},
			want: "Запрос отменён.",
		},
		{
			name: "skipped",
			request: func() *storage.Request {
				r := base()
				r.State = storage.StateSkipped
				return r
			},
			want: "Уже есть в Plex.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusLine(tc.request(), now, tc.candidates)
			if got != tc.want {
				t.Errorf("status line:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestStatusLineIsNeverEmpty — every state the interface can show says
// something. A blank line where a sentence should be is worse than a
// missing one: it looks like a page that forgot.
func TestStatusLineIsNeverEmpty(t *testing.T) {
	now := time.Now()

	for _, state := range storage.AllStates() {
		t.Run(string(state), func(t *testing.T) {
			r := sampleRequest()
			r.State = state
			r.NextRunAt = ptrTime(now.Add(time.Hour))

			if got := statusLine(r, now, 1); strings.TrimSpace(got) == "" {
				t.Error("the status line is empty")
			}
		})
	}
}

// TestTheBucketsCoverEveryActiveState — the three buckets are the whole of
// the active list. A state that is in none of them is a request the
// dashboard cannot show, which is a request nobody can cancel.
func TestTheBucketsCoverEveryActiveState(t *testing.T) {
	seen := map[storage.State]bool{}
	for _, b := range dashboardBuckets {
		for _, s := range b.States {
			if seen[s] {
				t.Errorf("the state %q is in two buckets", s)
			}
			seen[s] = true
		}
	}

	for _, s := range storage.ActiveStates() {
		if !seen[s] {
			t.Errorf("the state %q is in no bucket", s)
		}
	}
}

// ptrTime is a pointer to a moment, for the fixtures.
func ptrTime(t time.Time) *time.Time { return &t }
