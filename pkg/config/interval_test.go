package config_test

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
)

// cinnamonSchema is the Cinnamon applet's settings schema, relative to this
// package.
const cinnamonSchema = "../../extensions/cinnamon/sysmon-go@sysmon/settings-schema.json"

// intervalSetting is the part of the schema's update-interval entry these
// tests read.
type intervalSetting struct {
	Type    string         `json:"type"`
	Default int            `json:"default"`
	Options map[string]int `json:"options"`
}

// readIntervalSetting decodes the update-interval entry of the schema at path.
func readIntervalSetting(t *testing.T, path string) intervalSetting {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	entry, ok := schema["update-interval"]
	if !ok {
		t.Fatalf("%s has no update-interval setting", path)
	}
	var setting intervalSetting
	if err := json.Unmarshal(entry, &setting); err != nil {
		t.Fatalf("decode update-interval in %s: %v", path, err)
	}
	return setting
}

// TestCinnamonIntervalOptions_MatchAllowedIntervals: the applet offers exactly
// the intervals POST /api/interval accepts. It must be a combobox: Cinnamon
// keeps a combobox value across a schema upgrade when it is one of the
// options, but drops a spinbutton value that sits on min or max (its check is
// min < value < max), which reset 500 ms and 60 s to the default.
func TestCinnamonIntervalOptions_MatchAllowedIntervals(t *testing.T) {
	setting := readIntervalSetting(t, cinnamonSchema)

	if setting.Type != "combobox" {
		t.Errorf("update-interval type = %q, want combobox", setting.Type)
	}
	got := slices.Sorted(maps.Values(setting.Options))
	want := slices.Sorted(maps.Keys(config.AllowedIntervalMs))
	if !slices.Equal(got, want) {
		t.Errorf("update-interval options = %v, want the allowed intervals %v", got, want)
	}
	if !config.AllowedIntervalMs[setting.Default] {
		t.Errorf("update-interval default %d is not an allowed interval", setting.Default)
	}
}
