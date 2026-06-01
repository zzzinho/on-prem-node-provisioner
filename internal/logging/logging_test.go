package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/zzzinho/on-prem-node-provisioner/internal/logging"
)

func TestNewECSKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		level     slog.Leveler
		logLevel  slog.Level
		msg       string
		wantWrite bool
		wantLevel string
	}{
		{
			name:      "info at info level",
			level:     slog.LevelInfo,
			logLevel:  slog.LevelInfo,
			msg:       "hello",
			wantWrite: true,
			wantLevel: "INFO",
		},
		{
			name:      "error at warn level",
			level:     slog.LevelWarn,
			logLevel:  slog.LevelError,
			msg:       "boom",
			wantWrite: true,
			wantLevel: "ERROR",
		},
		{
			name:      "info filtered by warn level",
			level:     slog.LevelWarn,
			logLevel:  slog.LevelInfo,
			msg:       "quiet",
			wantWrite: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.New(logging.Options{Level: tt.level, Output: &buf})
			logger.Log(t.Context(), tt.logLevel, tt.msg)

			if !tt.wantWrite {
				if buf.Len() != 0 {
					t.Fatalf("expected no output, got %q", buf.String())
				}
				return
			}

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("output is not valid JSON: %v\nline: %q", err, buf.String())
			}

			if _, ok := rec["@timestamp"]; !ok {
				t.Errorf("missing @timestamp key in %v", rec)
			}
			if got, ok := rec["log.level"].(string); !ok || got != tt.wantLevel {
				t.Errorf("log.level = %v, want %q", rec["log.level"], tt.wantLevel)
			}
			if got, ok := rec["message"].(string); !ok || got != tt.msg {
				t.Errorf("message = %v, want %q", rec["message"], tt.msg)
			}

			// The renamed keys must not leak under their slog defaults.
			for _, k := range []string{slog.TimeKey, slog.LevelKey, slog.MessageKey} {
				if _, ok := rec[k]; ok {
					t.Errorf("unexpected default key %q in output", k)
				}
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "info", input: "info", want: slog.LevelInfo},
		{name: "warn", input: "warn", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "case insensitive", input: "WARN", want: slog.LevelWarn},
		{name: "surrounding space", input: "  info  ", want: slog.LevelInfo},
		{name: "unknown", input: "trace", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := logging.ParseLevel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
