// Package logging builds the application logger.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
)

// Module wires *slog.Logger into the fx graph.
var Module = fx.Module("logging", fx.Provide(New))

// New builds a logger from the configuration.
//
// It returns *slog.Logger rather than slog.Logger so that fx has a concrete
// pointer type to key the graph on; an interface would need an fx.As
// annotation at every provider that wanted it.
func New(cfg *config.Config) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Log.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Log.Format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("log.format: want text or json, got %q", cfg.Log.Format)
	}

	return slog.New(handler), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log.level: want debug, info, warn or error, got %q", s)
	}
}
