package types_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The browser suite renders these types from a fixture, and a fixture missing a
// field does not fail as a missing field: the component reads `undefined`,
// throws inside a formatter, and the test reports the label it was looking for
// as absent. That is what happened -- a hand-written container fixture had no
// `read_iops`, the detail drawer threw on `.toLocaleString()`, and the failure
// read as "CPU throttling not found".
//
// So the templates are generated from these structs, and this asserts they have
// not drifted. Adding a field to ContainerInfo now fails HERE, in Go, with the
// field named -- rather than in a Playwright timeout that names something else.
//
// Regenerate with the command in the failure message.
func TestFixtureTemplates_MatchTheTypesTheyStandIn(t *testing.T) {
	path := filepath.Join("..", "..", "cmd", "sysmon", "frontend", "tests", "fixtures", "entry-templates.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var fixture map[string]map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	tests := []struct {
		name  string
		value any
	}{
		{"container", types.ContainerInfo{}},
		{"vm", types.VMInfo{}},
		{"image", types.ImageInfo{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := jsonKeys(t, tt.value)
			got, ok := fixture[tt.name]
			if !ok {
				t.Fatalf("the fixture has no %q template", tt.name)
			}

			for _, key := range want {
				if _, present := got[key]; !present {
					t.Errorf("%q is missing from the %s template. A component reading it gets "+
						"undefined and throws inside a formatter, which surfaces as an unrelated "+
						"assertion failing. Regenerate the fixture from the Go types.", key, tt.name)
				}
			}
			for key := range got {
				if !slices.Contains(want, key) {
					t.Errorf("%q is in the %s template and not in the Go type; the fixture is "+
						"describing a field that no longer exists", key, tt.name)
				}
			}
		})
	}
}

// jsonKeys returns the field names v marshals to.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
