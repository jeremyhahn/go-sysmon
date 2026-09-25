package main

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strings"
	"testing"
)

// messages is a slog.Handler that keeps every record's level and message.
type messages struct{ lines *[]string }

func (m messages) Enabled(context.Context, slog.Level) bool { return true }
func (m messages) Handle(_ context.Context, r slog.Record) error {
	*m.lines = append(*m.lines, r.Level.String()+" "+r.Message)
	return nil
}
func (m messages) WithAttrs([]slog.Attr) slog.Handler { return m }
func (m messages) WithGroup(string) slog.Handler      { return m }

// TestApplyMemoryLimit_SaysWhatItDid verifies every outcome is logged once at
// startup and the runtime limit is set only when one was derived.
func TestApplyMemoryLimit_SaysWhatItDid(t *testing.T) {
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })

	tests := []struct {
		name      string
		root      func(t *testing.T) string
		getenv    func(string) string
		want      []string
		wantLimit bool
	}{
		{
			name:      "a cgroup watermark",
			root:      func(t *testing.T) string { return cgroupFixture(t, "1073741824", "max") },
			getenv:    noEnviron,
			want:      []string{"INFO memory limit: derived from the cgroup"},
			wantLimit: true,
		},
		{
			name:   "a watermark at the hard limit",
			root:   func(t *testing.T) string { return cgroupFixture(t, "1073741824", "1073741824") },
			getenv: noEnviron,
			want: []string{
				"WARN memory limit: MemoryHigh is at or above MemoryMax, so the throttle watermark is unreachable and the cgroup will be OOM-killed instead",
				"INFO memory limit: derived from the cgroup",
			},
			wantLimit: true,
		},
		{
			name:   "an operator's GOMEMLIMIT",
			root:   func(t *testing.T) string { return cgroupFixture(t, "1073741824", "max") },
			getenv: func(k string) string { return map[string]string{"GOMEMLIMIT": "512MiB"}[k] },
			want:   []string{"INFO memory limit: set by the environment"},
		},
		{
			name:   "no cgroup v2",
			root:   func(t *testing.T) string { return t.TempDir() },
			getenv: noEnviron,
			want:   []string{"INFO memory limit: no cgroup ceiling found; the Go runtime default applies"},
		},
		{
			name:   "an unreadable ceiling",
			root:   func(t *testing.T) string { return cgroupFixture(t, "lots", "max") },
			getenv: noEnviron,
			want:   []string{"WARN memory limit: cgroup unreadable, leaving the Go default in place"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debug.SetMemoryLimit(previous)
			var lines []string

			applyMemoryLimit(tt.root(t), tt.getenv, slog.New(messages{lines: &lines}))

			if strings.Join(lines, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("log =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(tt.want, "\n"))
			}
			if changed := debug.SetMemoryLimit(-1) != previous; changed != tt.wantLimit {
				t.Errorf("runtime memory limit changed = %v, want %v", changed, tt.wantLimit)
			}
		})
	}
}
