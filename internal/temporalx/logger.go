package temporalx

import (
	"log/slog"

	"go.temporal.io/sdk/log"
)

// temporalLogger adapts *slog.Logger to the interface the Temporal SDK expects.
//
// The SDK calls these with alternating key/value pairs rather than a map, so
// the pairs are converted on the way through. Calls come from the SDK's own
// goroutines, which is fine: slog handlers are safe for concurrent use.
type temporalLogger struct {
	l *slog.Logger
}

// newTemporalLogger returns an SDK logger that writes to the application log.
func newTemporalLogger(l *slog.Logger) log.Logger {
	return &temporalLogger{l: l.With("component", "temporal")}
}

func (t *temporalLogger) Debug(msg string, keyvals ...any) { t.l.Debug(msg, keyvals...) }
func (t *temporalLogger) Info(msg string, keyvals ...any)  { t.l.Info(msg, keyvals...) }
func (t *temporalLogger) Warn(msg string, keyvals ...any)  { t.l.Warn(msg, keyvals...) }
func (t *temporalLogger) Error(msg string, keyvals ...any) { t.l.Error(msg, keyvals...) }
