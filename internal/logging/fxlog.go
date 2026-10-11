package logging

import (
	"log/slog"
	"strings"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

// FXLogger returns an fx option that routes event logging to the application
// log at debug level.
//
// fx's stock logger prints every PROVIDE, SUPPLY and RUN line straight to
// stderr, which buries the application's own output under dependency-graph
// noise on every single start. Routing it through slog keeps it available when
// you are actually debugging the graph (log.level: debug) and out of the way
// otherwise.
//
// Nothing diagnostic is lost by hiding it: a construction failure is also
// returned from app.Start(), with the full dependency chain, so the error
// surfaces regardless of the log level.
func FXLogger() fx.Option {
	return fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
		return &fxevent.ConsoleLogger{W: &slogWriter{log: log}}
	})
}

type slogWriter struct {
	log *slog.Logger
}

func (w *slogWriter) Write(p []byte) (int, error) {
	// The console logger emits one event per Write, already newline-terminated.
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		w.log.Debug(line)
	}
	return len(p), nil
}
