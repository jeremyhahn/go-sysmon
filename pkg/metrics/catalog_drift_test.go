package metrics

import (
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/prom"
)

// TestCatalogMatchesWhatIsExported is the check that keeps the history catalog
// honest.
//
// pkg/prom/catalog.go names metrics by string, and nothing in the compiler
// connects those strings to what this file writes. A key naming a metric that
// is never produced draws an empty chart, which reads as "this node has no
// history for that" rather than as a mistake in a table -- and a splitting
// label that is never attached gives every line in a legend the same blank
// name. Both survive every other test in the repository.
//
// It lives here rather than in pkg/prom because the catalog is the dependent
// side: the metrics are the contract, and the catalog has to follow them.
func TestCatalogMatchesWhatIsExported(t *testing.T) {
	b := prom.NewBuilder(nil)
	Write(SampleSink{Builder: b, TimeMs: 1}, richSnapshot())

	names := make(map[string]bool)
	labels := make(map[string]map[string]bool)
	for _, s := range b.Drain() {
		name := ""
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				name = l.Value
			}
		}
		names[name] = true
		if labels[name] == nil {
			labels[name] = make(map[string]bool)
		}
		for _, l := range s.Labels {
			labels[name][l.Name] = true
		}
	}

	for _, m := range prom.Catalog() {
		if !names[m.Name] {
			t.Errorf("catalog key %q asks for %q, which this package never exports; "+
				"its chart would be permanently empty", m.Key, m.Name)
			continue
		}
		if m.SplitBy != "" && !labels[m.Name][m.SplitBy] {
			t.Errorf("catalog key %q splits %q by %q, a label it never carries; "+
				"every line in the legend would be unnamed", m.Key, m.Name, m.SplitBy)
		}
		for _, matcher := range m.Matchers {
			label := matcher[:indexOf(matcher, '=')]
			if !labels[m.Name][label] {
				t.Errorf("catalog key %q matches on %q, a label %q never carries; "+
					"the query would select nothing", m.Key, label, m.Name)
			}
		}
	}
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}

// TestReservedLabelsCoverWhatIsExported keeps config's reserved-label list in
// step with the labels this package actually attaches.
//
// pkg/config rejects a prometheus.external_labels entry whose name collides
// with one of go-sysmon's own, because a series carrying a label twice is
// rejected by Prometheus -- and it rejects the whole write request, so one such
// entry silently stops the node's entire history. The list has to be spelled
// out there, because configuration loading must not depend on the metric layer;
// this is where the drift would actually happen, so this is where it is caught.
func TestReservedLabelsCoverWhatIsExported(t *testing.T) {
	reserved := make(map[string]bool)
	for _, name := range config.ReservedLabelNames() {
		reserved[name] = true
	}

	b := prom.NewBuilder(nil)
	Write(SampleSink{Builder: b, TimeMs: 1}, richSnapshot())

	for _, s := range b.Drain() {
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				continue // structural, and already rejected by its __ prefix
			}
			if !reserved[l.Name] {
				t.Errorf("this package attaches the label %q, which "+
					"config.ReservedLabelNames() does not list; an operator "+
					"could set it in external_labels and silently stop every "+
					"write carrying it", l.Name)
			}
		}
	}
}
