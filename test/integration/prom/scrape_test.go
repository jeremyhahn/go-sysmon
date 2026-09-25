//go:build integration

package prom

import (
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// TestScrape_DescribesTheNodeAndNothingElse scrapes both builds of a node
// with no prometheus section: the node's own series, each declared once, and
// none of remote write's health series -- a node that does not push has no
// exporter to report on, so its exposition is what it always was.
func TestScrape_DescribesTheNodeAndNothingElse(t *testing.T) {
	for name, bin := range map[string]string{"server build": harness.Bins.Server, "default build": harness.Bins.Sysmon} {
		t.Run(name, func(t *testing.T) {
			s := harness.Serve(t, harness.ServeOptions{Bin: bin, Config: "fleet:\n  local_name: rack-7\n"})
			exposition := scrape(t, s)

			if v, ok := metricValue(exposition, "sysmon_up"); !ok || v != 1 {
				t.Errorf("sysmon_up = %v (present %v), want 1", v, ok)
			}
			for _, series := range []string{"sysmon_load1", "sysmon_memory_used_percent", "sysmon_psi_some_avg10"} {
				if n := strings.Count(exposition, "# TYPE "+series+" "); n != 1 {
					t.Errorf("%s is declared %d times, want once", series, n)
				}
			}
			if strings.Contains(exposition, "sysmon_remote_write_") {
				t.Errorf("a node that does not push reports remote write health:\n%s", exposition)
			}
			if v, ok := metricValue(exposition, "sysmon_alerts_firing", `severity="critical"`); !ok || v < 0 {
				t.Errorf("sysmon_alerts_firing{critical} = %v (present %v), want a count", v, ok)
			}
		})
	}
}

// TestHistory_OffAnswersDisabled verifies a node that keeps no history says
// so in the shape the dashboard hides its date picker on -- not a 404 --
// whether the build lacks prometheus support or the node has no query_url.
func TestHistory_OffAnswersDisabled(t *testing.T) {
	for name, bin := range map[string]string{"server build": harness.Bins.Server, "default build": harness.Bins.Sysmon} {
		t.Run(name, func(t *testing.T) {
			s := harness.Serve(t, harness.ServeOptions{Bin: bin, Config: "fleet:\n  local_name: rack-7\n"})

			var catalog types.HistoryCatalog
			harness.GetJSON(t, s.URL("/api/history/metrics"), &catalog)
			if catalog.Enabled || len(catalog.Metrics) != 0 {
				t.Errorf("GET /api/history/metrics = %+v, want history disabled", catalog)
			}
			var h types.History
			harness.GetJSON(t, s.URL("/api/history?metric=load1"), &h)
			if h.Enabled || len(h.Series) != 0 {
				t.Errorf("GET /api/history = %+v, want history disabled", h)
			}
		})
	}
}
