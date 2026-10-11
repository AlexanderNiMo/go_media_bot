package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreComplete(t *testing.T) {
	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The point of the defaults is that a bare binary works, so spot-check the
	// settings that would silently misbehave if they were zero.
	if cfg.Search.SearchDeadline != 17520*time.Hour {
		t.Errorf("search_deadline = %v, want 2 years", cfg.Search.SearchDeadline)
	}
	if cfg.Download.Deadline != 48*time.Hour {
		t.Errorf("download.deadline = %v, want 48h", cfg.Download.Deadline)
	}
	if len(cfg.Search.BackoffLadder) == 0 {
		t.Error("backoff_ladder is empty; the search loop would spin")
	}
	if cfg.Indexer.RatePerSecond <= 0 {
		t.Error("indexer.rate_per_second must be positive")
	}
	if cfg.Temporal.TaskQueue == "" {
		t.Error("temporal.task_queue is empty; activities would never be picked up")
	}
}

// TestEnvVarWithUnderscoreInKey covers the bug this loader was written to
// avoid: replacing every underscore in the variable name makes
// MEDIAD_POSTGRES_MAX_CONNS resolve to postgres.max.conns, which matches no
// key, so the setting silently keeps its default.
func TestEnvVarWithUnderscoreInKey(t *testing.T) {
	t.Setenv("MEDIAD_POSTGRES_MAX_CONNS", "42")
	t.Setenv("MEDIAD_SEARCH_CONTINUE_AS_NEW_HISTORY", "999")
	t.Setenv("MEDIAD_INDEXER_RATE_PER_SECOND", "0.5")

	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Postgres.MaxConns != 42 {
		t.Errorf("max_conns = %d, want 42", cfg.Postgres.MaxConns)
	}
	if cfg.Search.ContinueAsNewAt != 999 {
		t.Errorf("continue_as_new_history = %d, want 999", cfg.Search.ContinueAsNewAt)
	}
	if cfg.Indexer.RatePerSecond != 0.5 {
		t.Errorf("rate_per_second = %v, want 0.5", cfg.Indexer.RatePerSecond)
	}
}

// TestFileDurationsAndLists checks that a config file can express the values
// the defaults express in Go: durations as strings and lists as sequences.
func TestFileDurationsAndLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = `
search:
  search_deadline: 720h
  backoff_ladder: [30m, 3h]
  jitter: 5m
quality:
  min_seeders: 7
  excluded_sources: [CAM, TS]
plex:
  verify_timeout: 90m
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(Flags{ConfigFile: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Search.SearchDeadline != 720*time.Hour {
		t.Errorf("search_deadline = %v, want 720h", cfg.Search.SearchDeadline)
	}
	if got := cfg.Search.BackoffLadder; len(got) != 2 || got[0] != 30*time.Minute || got[1] != 3*time.Hour {
		t.Errorf("backoff_ladder = %v, want [30m 3h]", got)
	}
	if cfg.Plex.VerifyTimeout != 90*time.Minute {
		t.Errorf("verify_timeout = %v, want 90m", cfg.Plex.VerifyTimeout)
	}
	if cfg.Quality.MinSeeders != 7 {
		t.Errorf("min_seeders = %d, want 7", cfg.Quality.MinSeeders)
	}
	if got := cfg.Quality.ExcludedSources; len(got) != 2 || got[0] != "CAM" {
		t.Errorf("excluded_sources = %v, want [CAM TS]", got)
	}

	// Unset keys must keep their defaults rather than being zeroed by the
	// partial file.
	if cfg.Download.MaxStalls != 3 {
		t.Errorf("max_stalls = %d, want the default 3", cfg.Download.MaxStalls)
	}
}

// TestExplicitMissingFileIsAnError — a typo in --config must not be mistaken
// for "run on defaults".
func TestExplicitMissingFileIsAnError(t *testing.T) {
	if _, err := Load(Flags{ConfigFile: filepath.Join(t.TempDir(), "nope.yaml")}); err == nil {
		t.Fatal("expected an error for an explicitly named missing file")
	}
}

func TestValidationRejectsUnknownIndexerMode(t *testing.T) {
	t.Setenv("MEDIAD_INDEXER_MODE", "jacket")

	if _, err := Load(Flags{}); err == nil {
		t.Fatal("expected an error for an unknown indexer mode")
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	t.Setenv("MEDIAD_POSTGRES_DSN", "postgres://user:hunter2@db:5432/mediad")
	t.Setenv("MEDIAD_TMDB_API_KEY", "tmdb-secret")
	t.Setenv("MEDIAD_PLEX_TOKEN", "plex-secret")
	t.Setenv("MEDIAD_QBITTORRENT_PASSWORD", "qbit-secret")

	cfg, err := Load(Flags{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	red := cfg.Redacted()

	if got := red.Postgres.DSN; got != "postgres://user:***@db:5432/mediad" {
		t.Errorf("dsn = %q, want the password masked and the rest intact", got)
	}
	for name, got := range map[string]string{
		"tmdb.api_key":         red.TMDB.APIKey,
		"plex.token":           red.Plex.Token,
		"qbittorrent.password": red.QBittorrent.Password,
	} {
		if got != "***" {
			t.Errorf("%s = %q, want ***", name, got)
		}
	}

	// The original must be untouched: Redacted returns a copy.
	if cfg.Postgres.DSN != "postgres://user:hunter2@db:5432/mediad" {
		t.Error("Redacted mutated the receiver")
	}
}

func TestRedactDSNWithoutCredentials(t *testing.T) {
	// No password to hide; the DSN should survive rather than become "***",
	// otherwise the output stops being useful for debugging.
	if got := redactDSN("postgres://db:5432/mediad"); got != "postgres://db:5432/mediad" {
		t.Errorf("redactDSN = %q, want it unchanged", got)
	}
}
