// Package config loads runtime settings from a YAML file, the environment and
// command-line flags, in that order of increasing precedence.
//
// Secrets are expected to come from the environment (MEDIAD_*); the YAML file
// is for everything else. Every setting has a working default, so the binary
// starts with no configuration at all.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"go.uber.org/fx"
)

// Module wires the loaded configuration into the fx graph.
var Module = fx.Module("config", fx.Provide(Load))

// Config is the root of the settings tree.
//
// Each field carries both a koanf tag and a yaml tag. The koanf tag is what
// the loader matches against; the yaml tag exists so that `mediad config`
// prints keys that can be pasted straight back into config.yaml. Without it
// the YAML encoder falls back to the lowercased Go field name and emits
// `baseurl`, which loads as nothing at all.
type Config struct {
	Log         Log         `koanf:"log"         yaml:"log"`
	Server      Server      `koanf:"server"      yaml:"server"`
	Postgres    Postgres    `koanf:"postgres"    yaml:"postgres"`
	Temporal    Temporal    `koanf:"temporal"    yaml:"temporal"`
	TMDB        TMDB        `koanf:"tmdb"        yaml:"tmdb"`
	Plex        Plex        `koanf:"plex"        yaml:"plex"`
	Indexer     Indexer     `koanf:"indexer"     yaml:"indexer"`
	QBittorrent QBittorrent `koanf:"qbittorrent" yaml:"qbittorrent"`
	Telegram    Telegram    `koanf:"telegram"    yaml:"telegram"`
	Search      Search      `koanf:"search"      yaml:"search"`
	Download    Download    `koanf:"download"    yaml:"download"`
	Quality     Quality     `koanf:"quality"     yaml:"quality"`
}

// Log controls the slog handler.
type Log struct {
	Level  string `koanf:"level"  yaml:"level"`  // debug | info | warn | error
	Format string `koanf:"format" yaml:"format"` // text | json
}

// Server is the HTTP front door.
type Server struct {
	Listen  string `koanf:"listen"   yaml:"listen"`
	BaseURL string `koanf:"base_url" yaml:"base_url"`
}

// Postgres holds the application database connection settings.
//
// This is the mediad database, not the two Temporal owns. Migrations for this
// database are applied by the application on startup.
type Postgres struct {
	DSN      string `koanf:"dsn"       yaml:"dsn"`
	MaxConns int32  `koanf:"max_conns" yaml:"max_conns"`
}

// Temporal points at the workflow server.
type Temporal struct {
	Host      string `koanf:"host"       yaml:"host"`
	Namespace string `koanf:"namespace"  yaml:"namespace"`
	TaskQueue string `koanf:"task_queue" yaml:"task_queue"`
}

// TMDB is the metadata source used for search and for season listings.
type TMDB struct {
	APIKey   string        `koanf:"api_key"   yaml:"api_key"`
	Language string        `koanf:"language"  yaml:"language"`
	CacheTTL time.Duration `koanf:"cache_ttl" yaml:"cache_ttl"`
}

// Plex is the media server whose library we check before downloading anything.
type Plex struct {
	URL           string        `koanf:"url"            yaml:"url"`
	Token         string        `koanf:"token"          yaml:"token"`
	SectionsTTL   time.Duration `koanf:"sections_ttl"   yaml:"sections_ttl"`   // in-memory cache of the section list
	CheckTTL      time.Duration `koanf:"check_ttl"      yaml:"check_ttl"`      // how often to re-check during a search loop
	VerifyTimeout time.Duration `koanf:"verify_timeout" yaml:"verify_timeout"` // how long to wait for an item to appear after download
}

// Indexer selects and configures the torrent indexer front end.
type Indexer struct {
	Mode          string        `koanf:"mode"            yaml:"mode"` // prowlarr | torznab
	BaseURL       string        `koanf:"base_url"        yaml:"base_url"`
	APIKey        string        `koanf:"api_key"         yaml:"api_key"`
	IndexerIDs    []int         `koanf:"indexer_ids"     yaml:"indexer_ids"` // prowlarr only; empty means all
	RatePerSecond float64       `koanf:"rate_per_second" yaml:"rate_per_second"`
	CapsTTL       time.Duration `koanf:"caps_ttl"        yaml:"caps_ttl"`
}

// QBittorrent is the torrent client.
//
// SavePath is deliberately absent: the client owns the download location.
type QBittorrent struct {
	URL string `koanf:"url" yaml:"url"`

	// APIKey authenticates with a bearer token, which qBittorrent 5.1 added.
	// When set it takes precedence over the username and password, and no
	// login request is made at all — which also removes the failure mode where
	// repeated failed logins get the client's IP banned.
	APIKey string `koanf:"api_key" yaml:"api_key"`

	Username string `koanf:"username" yaml:"username"`
	Password string `koanf:"password" yaml:"password"`

	Category     string        `koanf:"category"      yaml:"category"`
	PollInterval time.Duration `koanf:"poll_interval" yaml:"poll_interval"`
}

// Telegram is the chat front end.
//
// Long polling is used rather than webhooks: the service is self-hosted and is
// not expected to be reachable from the internet.
type Telegram struct {
	Enabled bool   `koanf:"enabled" yaml:"enabled"`
	Token   string `koanf:"token"   yaml:"token"`
}

// Search controls the torrent search loop.
//
// There is deliberately no attempt limit. Over a two-year horizon any fixed
// count is reached and then becomes the real deadline; BackoffLadder is what
// bounds the request rate instead.
type Search struct {
	SearchDeadline  time.Duration   `koanf:"search_deadline"          yaml:"search_deadline"`
	BackoffLadder   []time.Duration `koanf:"backoff_ladder"           yaml:"backoff_ladder"`
	Jitter          time.Duration   `koanf:"jitter"                   yaml:"jitter"`
	PollInterval    time.Duration   `koanf:"poll_interval"            yaml:"poll_interval"`
	ContinueAsNewAt int             `koanf:"continue_as_new_history"  yaml:"continue_as_new_history"`
}

// Download bounds a single torrent attempt.
type Download struct {
	Deadline       time.Duration `koanf:"deadline"          yaml:"deadline"`
	ZeroStallAfter time.Duration `koanf:"zero_stall_after"  yaml:"zero_stall_after"`
	// MissingGrace is how long a torrent may be absent from the client before
	// that counts as gone. A single miss proves nothing: an add is
	// asynchronous, and a client that is restarting reports nothing for a
	// moment.
	MissingGrace time.Duration `koanf:"missing_grace" yaml:"missing_grace"`
	MaxStalls    int           `koanf:"max_stalls"    yaml:"max_stalls"`
}

// Quality is the release acceptance policy.
type Quality struct {
	MinResolution       int      `koanf:"min_resolution"       yaml:"min_resolution"`
	PreferredResolution int      `koanf:"preferred_resolution" yaml:"preferred_resolution"`
	MinSeeders          int      `koanf:"min_seeders"          yaml:"min_seeders"`
	MinSizeGB           float64  `koanf:"min_size_gb"          yaml:"min_size_gb"`
	MaxSizeGB           float64  `koanf:"max_size_gb"          yaml:"max_size_gb"`
	ExcludedSources     []string `koanf:"excluded_sources"     yaml:"excluded_sources"`
	RequireRussianAudio bool     `koanf:"russian_audio"        yaml:"russian_audio"`
	RussianMarkers      []string `koanf:"russian_markers"      yaml:"russian_markers"`
}

// Flags carries values that come from cobra rather than from the file, since
// flags are parsed after the config is loaded.
type Flags struct {
	ConfigFile string
}

// Module-like provider input. Load reads the file named by the --config flag,
// falling back to config.yaml in the working directory when it exists.
func Load(flags Flags) (*Config, error) {
	k := koanf.New(".")

	// A missing default file is not an error: the project runs on defaults
	// plus environment variables. A missing file that was named explicitly is
	// an error, because silently ignoring it would hide a typo.
	path := flags.ConfigFile
	optional := path == ""
	if optional {
		path = "config.yaml"
	}

	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		if !optional {
			return nil, fmt.Errorf("load config file %q: %w", path, err)
		}
	}

	// MEDIAD_POSTGRES_DSN -> postgres.dsn
	//
	// Only the first underscore becomes a dot. The env provider's usual
	// recipe — replace every underscore — would turn
	// MEDIAD_POSTGRES_MAX_CONNS into postgres.max.conns, which matches
	// nothing, and the setting would silently keep its default. Every key in
	// this configuration is exactly two levels deep, so splitting on the first
	// separator is unambiguous.
	if err := k.Load(env.Provider("MEDIAD_", ".", func(s string) string {
		return strings.Replace(strings.ToLower(strings.TrimPrefix(s, "MEDIAD_")), "_", ".", 1)
	}), nil); err != nil {
		return nil, fmt.Errorf("load environment: %w", err)
	}

	cfg := defaults()
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// defaults returns a fully populated Config. Unmarshalling over it means a
// partially specified file or environment only overrides what it names.
func defaults() Config {
	return Config{
		Log: Log{Level: "info", Format: "text"},
		Server: Server{
			// 8081, not the more usual 8080: qBittorrent's web UI defaults to
			// 8080, and this service is expected to run on the same host as it.
			Listen:  "0.0.0.0:8081",
			BaseURL: "http://localhost:8081",
		},
		Postgres: Postgres{
			DSN:      "postgres://mediad:mediad@localhost:5432/mediad?sslmode=disable",
			MaxConns: 10,
		},
		Temporal: Temporal{
			Host:      "localhost:7233",
			Namespace: "mediad",
			TaskQueue: "mediad",
		},
		TMDB: TMDB{
			Language: "ru-RU",
			CacheTTL: 24 * time.Hour,
		},
		Plex: Plex{
			URL:           "http://localhost:32400",
			SectionsTTL:   time.Hour,
			CheckTTL:      6 * time.Hour,
			VerifyTimeout: 6 * time.Hour,
		},
		Indexer: Indexer{
			Mode:    "prowlarr",
			BaseURL: "http://localhost:9696",
			// 1 request per 4s. Deliberately below the 1-per-3s target: the
			// server spreads activity tasks across roughly four partitions of
			// a task queue and can burst above the configured average, and a
			// burst that trips a tracker's rate limit gets the IP banned.
			RatePerSecond: 0.25,
			CapsTTL:       24 * time.Hour,
		},
		QBittorrent: QBittorrent{
			URL:          "http://localhost:8080",
			Category:     "mediad",
			PollInterval: 30 * time.Second,
		},
		Search: Search{
			SearchDeadline: 17520 * time.Hour, // 2 years
			BackoffLadder: []time.Duration{
				time.Hour, 2 * time.Hour, 4 * time.Hour,
				8 * time.Hour, 12 * time.Hour, 24 * time.Hour,
			},
			Jitter: 10 * time.Minute,
			// The user interface needs progress every 30 seconds; the workflow
			// only needs to know done/stalled/expired. Polling the workflow at
			// 30s for a two-day download would cost ~50k history events, which
			// is the termination limit.
			PollInterval:    5 * time.Minute,
			ContinueAsNewAt: 5000,
		},
		Download: Download{
			Deadline:       48 * time.Hour,
			ZeroStallAfter: 45 * time.Minute,
			MissingGrace:   3 * time.Minute,
			MaxStalls:      3,
		},
		Quality: Quality{
			MinResolution:       1080,
			PreferredResolution: 2160,
			MinSeeders:          3,
			MinSizeGB:           1.5,
			MaxSizeGB:           60,
			ExcludedSources:     []string{"CAM", "TS", "TELESYNC", "HDTS", "DVDSCR"},
			RequireRussianAudio: true,
		},
	}
}

func (c *Config) validate() error {
	switch c.Indexer.Mode {
	case "prowlarr", "torznab":
	default:
		return fmt.Errorf("indexer.mode: want prowlarr or torznab, got %q", c.Indexer.Mode)
	}
	if c.Postgres.DSN == "" {
		return fmt.Errorf("postgres.dsn must not be empty")
	}
	if c.Temporal.TaskQueue == "" {
		return fmt.Errorf("temporal.task_queue must not be empty")
	}
	if c.Search.ContinueAsNewAt <= 0 {
		return fmt.Errorf("search.continue_as_new_history must be positive")
	}
	return nil
}

// Redacted returns a copy safe to log or print: every secret is replaced.
func (c Config) Redacted() Config {
	if c.Postgres.DSN != "" {
		c.Postgres.DSN = redactDSN(c.Postgres.DSN)
	}
	c.TMDB.APIKey = redact(c.TMDB.APIKey)
	c.Plex.Token = redact(c.Plex.Token)
	c.Indexer.APIKey = redact(c.Indexer.APIKey)
	c.QBittorrent.Password = redact(c.QBittorrent.Password)
	c.QBittorrent.APIKey = redact(c.QBittorrent.APIKey)
	if c.Telegram.Token != "" {
		c.Telegram.Token = redact(c.Telegram.Token)
	}
	return c
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// redactDSN hides the password in a postgres URL while leaving the rest
// readable, which is what makes the output useful for debugging.
//
// A DSN with no credentials at all is returned unchanged: there is nothing to
// hide, and replacing it with "***" would throw away the host and database
// name that make the output worth printing.
func redactDSN(dsn string) string {
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return "***"
	}

	rest := dsn[scheme+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return dsn // no userinfo section
	}

	creds := rest[:at]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return dsn // a username but no password
	}

	return dsn[:scheme+3] + creds[:colon] + ":***" + dsn[scheme+3+at:]
}
