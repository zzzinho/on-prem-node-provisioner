// Package logging builds the single structured logger used across ONP.
//
// Every component logs JSON to stdout so a Filebeat sidecar can ship lines to
// Elasticsearch without a parsing step. The top-level keys are renamed to the
// Elastic Common Schema (ECS) shape — @timestamp, log.level, message — so the
// records line up with the rest of the cluster's logs out of the box.
//
// The logger is plain slog from the standard library. controller-runtime and
// klog speak logr, so we bridge through logr.FromSlogHandler rather than
// pulling zap into our own code.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
)

// Options configures the logger built by New. The zero value is usable: it
// logs at Info to stdout without source positions.
type Options struct {
	// Level is the minimum level to emit. A nil Leveler means slog.LevelInfo.
	Level slog.Leveler
	// Output is where lines are written. A nil Output means os.Stdout.
	Output io.Writer
	// AddSource includes the calling file and line under "source".
	AddSource bool
}

// New returns a JSON slog.Logger with ECS-aligned top-level keys.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}

	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:       opts.Level,
		AddSource:   opts.AddSource,
		ReplaceAttr: ecsKeys,
	})
	return slog.New(handler)
}

// ecsKeys renames slog's built-in top-level keys to their ECS equivalents.
//
// Only the root group (len(groups) == 0) is touched: nested attributes that
// happen to be named "time"/"level"/"msg" keep their names. The values are
// left untouched — in particular the timestamp stays slog's default RFC3339
// marshaling, which Elasticsearch's date detection accepts. The dotted keys
// (log.level) stay flat; Elasticsearch expands them into nested objects at
// index time, so no client-side nesting is required.
func ecsKeys(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "@timestamp"
	case slog.LevelKey:
		a.Key = "log.level"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// Logr adapts an slog.Logger into a logr.Logger for libraries that log through
// logr — namely controller-runtime and klog. Both share the same underlying
// handler, so all records flow through one JSON encoder.
func Logr(l *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(l.Handler())
}

// ParseLevel maps a --log-level flag value to an slog.Level. The match is
// case-insensitive and accepts the four canonical names.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q (want debug|info|warn|error)", s)
	}
}
