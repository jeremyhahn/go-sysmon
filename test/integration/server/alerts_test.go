//go:build integration

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/alert"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// alertsOf returns the alerts on the server's latest snapshot, by rule.
func alertsOf(t *testing.T, s *harness.Server) map[string][]types.Alert {
	t.Helper()
	var snap types.Snapshot
	harness.GetJSON(t, s.URL("/api/snapshot"), &snap)
	byRule := make(map[string][]types.Alert)
	for _, a := range snap.Alerts {
		byRule[a.Rule] = append(byRule[a.Rule], a)
	}
	return byRule
}

// awaitAlerts waits for the snapshot's alerts to satisfy cond, and returns
// them.
func awaitAlerts(t *testing.T, s *harness.Server, what string, cond func(map[string][]types.Alert) bool) map[string][]types.Alert {
	t.Helper()
	var got map[string][]types.Alert
	await.Until(t, what, func(context.Context) bool {
		got = alertsOf(t, s)
		return cond(got)
	})
	return got
}

// TestAlerts_TheRealHostRaisesAndClearsThem configures a threshold every
// host crosses -- memory in use above 0.01% -- so the alert comes from a real
// reading. It is raised with the configured threshold and the reading that
// crossed it, counted in the summary a parent polls, kept with its original
// time while it stands, and cleared by a reload that disables the rule.
func TestAlerts_TheRealHostRaisesAndClearsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("alerts:\n  memory_pct: 0.01\n")
	s := harness.Serve(t, harness.ServeOptions{ConfigPath: path})

	got := awaitAlerts(t, s, "the memory alert", func(a map[string][]types.Alert) bool {
		return len(a[alert.RuleMemoryHigh]) > 0
	})
	mem := got[alert.RuleMemoryHigh][0]
	if mem.Threshold != 0.01 || mem.Value <= mem.Threshold || mem.Since.IsZero() || mem.Message == "" ||
		mem.Severity == "" || mem.Category == "" || mem.ID == "" {
		t.Errorf("memory alert = %+v, want the reading over the 0.01 threshold, identified, dated and described", mem)
	}

	var summary types.NodeSummary
	harness.GetJSON(t, s.URL("/api/summary"), &summary)
	if summary.WarningCount+summary.CriticalCount == 0 || len(summary.Alerts) == 0 {
		t.Errorf("summary counts %d critical, %d warning; want the alert a parent must see",
			summary.CriticalCount, summary.WarningCount)
	}

	// Still standing a collection later: the same alert, from the same time.
	later := awaitAlerts(t, s, "a later collection", func(a map[string][]types.Alert) bool {
		return len(a[alert.RuleMemoryHigh]) > 0 && a[alert.RuleMemoryHigh][0].Value != mem.Value
	})
	if still := later[alert.RuleMemoryHigh][0]; !still.Since.Equal(mem.Since) || still.ID != mem.ID {
		t.Errorf("the standing alert became %+v, want %s since %v", still, mem.ID, mem.Since)
	}

	write("alerts:\n  memory_pct: 0\n")
	s.Signal(t, syscall.SIGHUP)
	awaitAlerts(t, s, "the disabled alert to clear", func(a map[string][]types.Alert) bool {
		return len(a[alert.RuleMemoryHigh]) == 0
	})
}

// TestAlerts_AThresholdThatCannotBeUsedStopsTheService verifies alert
// settings that cannot be what the operator meant -- a misspelt rule, one
// this build has retired, a threshold that is not a number -- stop the
// service at startup with the key named, instead of running with the rule
// silently off.
func TestAlerts_AThresholdThatCannotBeUsedStopsTheService(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"a misspelt rule":      {"alerts:\n  memory_percent: 90\n", "memory_percent"},
		"a retired rule":       {"alerts:\n  swap_pct: 50\n", "swap_out_pages_per_sec"},
		"a threshold in words": {"alerts:\n  memory_pct: lots\n", "memory_pct"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := harness.WriteFile(t, "sysmon.yaml", c.body)
			r := harness.Run(t, harness.Bins.Sysmon, nil, "serve", "--addr", "127.0.0.1:0", "--config", path)
			if r.Code == 0 {
				t.Fatalf("serve with %s exited 0", name)
			}
			if !strings.Contains(r.Output(), c.want) {
				t.Errorf("serve with %s said:\n%s\nwant it to name %q", name, r.Output(), c.want)
			}
		})
	}
}
