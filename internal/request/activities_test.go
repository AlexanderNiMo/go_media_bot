package request

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alex/mediad/internal/domain"
	"github.com/alex/mediad/internal/indexer"
	"github.com/alex/mediad/internal/release"
)

// The search activity and the candidate list.
//
// A search that finds nothing acceptable is the ordinary outcome of most
// searches, and the releases it refused are what the user picks from when
// the auto-selector cannot help. These tests pin that the list is written,
// that it says how many there are, and that it says so only once.

func TestSearchReleasesStoresTheRejectedCandidates(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	rid := insertRequest(t, pool, "movie-603-candidates")

	acts := &Activities{
		store:   store,
		indexer: &fakeIndexer{releases: rejectedReleases()},
		profile: release.DefaultProfile(),
		log:     slog.New(slog.DiscardHandler),
	}

	res, err := acts.SearchReleases(ctx, SearchInput{
		RequestID: rid,
		Spec:      movieSpec(),
		Limit:     100,
	})
	if err != nil {
		t.Fatalf("SearchReleases: %v", err)
	}

	if res.Found() {
		t.Fatalf("the search found a release, but every release in the fixture is unacceptable")
	}
	if res.Candidates != 3 {
		t.Errorf("Candidates = %d, want 3", res.Candidates)
	}
	if !res.CandidatesNew {
		t.Error("CandidatesNew is false, but this is the first list for this request")
	}

	stored, err := store.ListCandidates(ctx, rid)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("the list has %d rows, want 3", len(stored))
	}
	// The list is what the user picks from, so it has to carry the magnet
	// and the reason the release was refused. A row without them is a row
	// the user cannot act on.
	for _, c := range stored {
		if c.MagnetLink == "" {
			t.Errorf("candidate %d has no magnet link", c.Rank)
		}
		if c.RejectionReason == "" {
			t.Errorf("candidate %d has no rejection reason", c.Rank)
		}
	}
	if stored[0].Rank != 1 || stored[2].Rank != 3 {
		t.Errorf("the list is not in rank order: %v", stored)
	}
}

func TestSearchReleasesRefreshesTheListWithoutRenotifying(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	rid := insertRequest(t, pool, "movie-603-refresh")

	acts := &Activities{
		store:   store,
		indexer: &fakeIndexer{releases: rejectedReleases()},
		profile: release.DefaultProfile(),
		log:     slog.New(slog.DiscardHandler),
	}

	first, err := acts.SearchReleases(ctx, SearchInput{RequestID: rid, Spec: movieSpec(), Limit: 100})
	if err != nil {
		t.Fatalf("SearchReleases: %v", err)
	}
	if !first.CandidatesNew {
		t.Error("the first search did not report a fresh list")
	}

	second, err := acts.SearchReleases(ctx, SearchInput{RequestID: rid, Spec: movieSpec(), Limit: 100})
	if err != nil {
		t.Fatalf("SearchReleases (second): %v", err)
	}
	if second.CandidatesNew {
		t.Error("the second search reported a fresh list, so the user would be told twice")
	}
	if second.Candidates != 3 {
		t.Errorf("Candidates = %d, want 3", second.Candidates)
	}
}

func TestSearchReleasesClearsTheListWhenItFindsARelease(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	rid := insertRequest(t, pool, "movie-603-found")

	acts := &Activities{
		store:   store,
		indexer: &fakeIndexer{releases: rejectedReleases()},
		profile: release.DefaultProfile(),
		log:     slog.New(slog.DiscardHandler),
	}
	if _, err := acts.SearchReleases(ctx, SearchInput{RequestID: rid, Spec: movieSpec(), Limit: 100}); err != nil {
		t.Fatalf("SearchReleases: %v", err)
	}

	// The same indexer, but this time with a release the selector accepts.
	acts.indexer = &fakeIndexer{releases: append(rejectedReleases(), acceptableRelease())}
	res, err := acts.SearchReleases(ctx, SearchInput{RequestID: rid, Spec: movieSpec(), Limit: 100})
	if err != nil {
		t.Fatalf("SearchReleases (second): %v", err)
	}
	if !res.Found() {
		t.Fatal("the acceptable release was not selected")
	}

	// A list of what was refused is noise once something was accepted:
	// the request is downloading.
	stored, err := store.ListCandidates(ctx, rid)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("the list has %d rows after a release was accepted, want 0", len(stored))
	}
}

// fakeIndexer returns a fixed list of releases.
type fakeIndexer struct {
	releases []domain.Release
}

// TestSearchSeasonUpdatesPicksTheUpdateOfTheTrackedPack — the search a
// request makes while it waits for the rest of a season.
//
// Only the release from the tracked pack's own thread, with more episodes in
// it, is an update. A better release of the same season from somewhere else
// is not what the request is waiting for.
func TestSearchSeasonUpdatesPicksTheUpdateOfTheTrackedPack(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	rid := insertRequest(t, pool, "tv-1399-s2-update")

	acts := &Activities{
		store:   store,
		indexer: &fakeIndexer{releases: seasonReleases()},
		profile: release.DefaultProfile(),
		log:     slog.New(slog.DiscardHandler),
	}

	res, err := acts.SearchSeasonUpdates(ctx, UpdateSearchInput{
		RequestID: rid,
		Spec:      seasonSpec(),
		Tracked: release.Tracked{
			ThreadID: "rutracker:12345",
			Episodes: 8,
			Title:    "Игра престолов S02 1080p (1-8 из 10)",
		},
	})
	if err != nil {
		t.Fatalf("SearchSeasonUpdates: %v", err)
	}

	if !res.Found() {
		t.Fatalf("no update was found: %s", res.Summary)
	}
	if res.Chosen.Title != "Игра престолов S02 1080p (1-10 из 12)" {
		t.Errorf("chose %q", res.Chosen.Title)
	}

	// The releases that are not updates are not options for this request,
	// so they are not offered to the user either.
	stored, err := store.ListCandidates(ctx, rid)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("the list has %d rows, want 0: a request waiting for the rest of a "+
			"season is not looking for a different release", len(stored))
	}
}

// seasonReleases are the releases an indexer returns for a season that is
// still being released: the update, the pack the request is already
// following, and a pack of the same season from another upload.
func seasonReleases() []domain.Release {
	return []domain.Release{
		{
			Title:       "Игра престолов S02 1080p (1-10 из 12)",
			Indexer:     "rutracker",
			ThreadID:    "rutracker:12345",
			InfoHash:    "e5",
			MagnetURI:   "magnet:?xt=urn:btih:e5",
			SizeBytes:   25 << 30,
			Seeders:     50,
			DownloadURL: "https://tracker/dl/e5",
		},
		{
			Title:       "Игра престолов S02 1080p (1-8 из 10)",
			Indexer:     "rutracker",
			ThreadID:    "rutracker:12345",
			InfoHash:    "f6",
			MagnetURI:   "magnet:?xt=urn:btih:f6",
			SizeBytes:   20 << 30,
			Seeders:     40,
			DownloadURL: "https://tracker/dl/f6",
		},
		{
			Title:       "Игра престолов S02 720p (1-12 из 12)",
			Indexer:     "rutor",
			ThreadID:    "rutor:99999",
			InfoHash:    "a7",
			MagnetURI:   "magnet:?xt=urn:btih:a7",
			SizeBytes:   12 << 30,
			Seeders:     60,
			DownloadURL: "https://tracker/dl/a7",
		},
	}
}

func (f *fakeIndexer) Search(context.Context, indexer.SearchRequest) ([]domain.Release, error) {
	return f.releases, nil
}

func (f *fakeIndexer) Caps(context.Context) (indexer.Caps, error) { return indexer.Caps{}, nil }

func (f *fakeIndexer) Name() string { return "fake" }

// rejectedReleases are three releases the default profile refuses: a dub
// that is right, a resolution that is right, but not enough seeders.
func rejectedReleases() []domain.Release {
	return []domain.Release{
		{
			Title:       "Матрица (1999) 1080p BDRemux | Дубляж",
			Indexer:     "rutor",
			InfoHash:    "a1",
			MagnetURI:   "magnet:?xt=urn:btih:a1",
			SizeBytes:   20 << 30,
			Seeders:     0,
			DownloadURL: "https://tracker/dl/a1",
		},
		{
			Title:       "Матрица (1999) 1080p WEB-DL | Дубляж",
			Indexer:     "rutor",
			InfoHash:    "b2",
			MagnetURI:   "magnet:?xt=urn:btih:b2",
			SizeBytes:   8 << 30,
			Seeders:     1,
			DownloadURL: "https://tracker/dl/b2",
		},
		{
			Title:       "Матрица (1999) 1080p BDRip | Дубляж",
			Indexer:     "rutor",
			InfoHash:    "c3",
			MagnetURI:   "magnet:?xt=urn:btih:c3",
			SizeBytes:   10 << 30,
			Seeders:     2,
			DownloadURL: "https://tracker/dl/c3",
		},
	}
}

// acceptableRelease passes every hard filter of the default profile.
func acceptableRelease() domain.Release {
	return domain.Release{
		Title:       "Матрица (1999) 1080p BDRemux | Дубляж",
		Indexer:     "rutor",
		InfoHash:    "d4",
		MagnetURI:   "magnet:?xt=urn:btih:d4",
		SizeBytes:   20 << 30,
		Seeders:     50,
		DownloadURL: "https://tracker/dl/d4",
	}
}

// insertRequest adds a request row for the candidate list to hang off.
func insertRequest(t *testing.T, pool *pgxpool.Pool, workflowID string) int64 {
	t.Helper()

	mediaID := insertMedia(t, pool, "movie", 603, nil, "The Matrix")
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO requests (media_id, workflow_id, state, search_deadline)
		VALUES ($1, $2, 'pending', now() + interval '2 years')
		RETURNING id
	`, mediaID, workflowID).Scan(&id)
	if err != nil {
		t.Fatalf("insert request: %v", err)
	}
	return id
}

// Compile-time check that the fake satisfies the interface the activity
// holds. The activity takes the interface, not the concrete type, so a
// mismatch here would only show up as a runtime failure.
var _ indexer.Indexer = (*fakeIndexer)(nil)
