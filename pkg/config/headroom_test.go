package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// TestLoad_HeadroomTargetDefaultsToEightyPercent: a file that does not set
// fleet.headroom_target gets 0.8 (#239), attributed to the defaults.
func TestLoad_HeadroomTargetDefaultsToEightyPercent(t *testing.T) {
	cfg, sources, err := config.NewLoader(filepath.Join(t.TempDir(), "absent.yaml")).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := cfg.Fleet.HeadroomTarget(); got != 0.8 {
		t.Errorf("HeadroomTarget() = %v, want 0.8", got)
	}
	if got := sources["fleet.headroom_target"]; got != types.ConfigSourceDefault {
		t.Errorf("source = %q, want %q", got, types.ConfigSourceDefault)
	}
}

// TestLoad_HeadroomTargetFromTheFile: a value in (0, 1] is taken as written,
// the upper bound included -- a coordinator may run nodes flat out.
func TestLoad_HeadroomTargetFromTheFile(t *testing.T) {
	for _, tc := range []struct {
		body string
		want float64
	}{
		{"fleet:\n  headroom_target: 0.65\n", 0.65},
		{"fleet:\n  headroom_target: 1\n", 1},
		{"fleet:\n  headroom_target: 0.01\n", 0.01},
	} {
		cfg := load(t, tc.body)
		if got := cfg.Fleet.HeadroomTarget(); got != tc.want {
			t.Errorf("%q: HeadroomTarget() = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestLoad_HeadroomTargetOutOfRangeIsRefused: a target that is not a
// utilisation fraction is an error naming the key, never silently repaired --
// the file would otherwise state a target the fleet view does not use. Zero
// is refused too: an unset key already reads as the default, so a zero was
// written.
func TestLoad_HeadroomTargetOutOfRangeIsRefused(t *testing.T) {
	for _, v := range []string{"0", "-0.2", "1.01", "80", ".nan"} {
		t.Run(v, func(t *testing.T) {
			_, _, err := config.NewLoader(writeConfig(t, "fleet:\n  headroom_target: "+v+"\n")).Load()
			var target *config.HeadroomTargetError
			if !errors.As(err, &target) {
				t.Fatalf("Load() = %v, want a *HeadroomTargetError", err)
			}
		})
	}
}

// TestHeadroomTargetError_NamesTheKeyAndTheRange: the message is what an
// operator reads in the log, so it says which key and what is allowed.
func TestHeadroomTargetError_NamesTheKeyAndTheRange(t *testing.T) {
	msg := (&config.HeadroomTargetError{Value: 1.5}).Error()
	want := "config: fleet.headroom_target 1.5 is out of range; it is a utilisation fraction, " +
		"greater than 0 and at most 1 (0.8 is 80%)"
	if msg != want {
		t.Errorf("Error() = %q\nwant      %q", msg, want)
	}
}

// TestFleet_ZeroValueHeadroomTargetIsTheDefault: a config.Fleet built in code
// without Defaults -- by a library embedding go-sysmon, or a test -- computes
// headroom at the default, not at a target of nothing.
func TestFleet_ZeroValueHeadroomTargetIsTheDefault(t *testing.T) {
	if got := (config.Fleet{}).HeadroomTarget(); got != config.DefaultHeadroomTarget {
		t.Errorf("zero Fleet HeadroomTarget() = %v, want %v", got, config.DefaultHeadroomTarget)
	}
	if got := (config.Fleet{HeadroomTargetFraction: 0.5}).HeadroomTarget(); got != 0.5 {
		t.Errorf("HeadroomTarget() = %v, want the configured 0.5", got)
	}
}

// TestManagerReload_HeadroomTargetIsReloadable: like the other fleet keys, a
// new target is live after a reload and shown in the settings view; an
// out-of-range one keeps the running configuration.
func TestManagerReload_HeadroomTargetIsReloadable(t *testing.T) {
	path := writeConfig(t, "fleet:\n  headroom_target: 0.8\n")
	mgr, err := config.NewManager(l(path))
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}

	if err := os.WriteFile(path, []byte("fleet:\n  headroom_target: 0.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload(): %v", err)
	}
	if got := mgr.Current().Fleet.HeadroomTarget(); got != 0.6 {
		t.Errorf("after reload HeadroomTarget() = %v, want 0.6", got)
	}
	if entry := viewEntry(t, mgr, "fleet.headroom_target"); entry.Value != "0.6" || entry.Source != types.ConfigSourceFile {
		t.Errorf("view = %+v, want 0.6 from the config file", entry)
	}

	if err := os.WriteFile(path, []byte("fleet:\n  headroom_target: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reload(); err == nil {
		t.Fatal("Reload() with headroom_target 2 = nil, want an error")
	}
	if got := mgr.Current().Fleet.HeadroomTarget(); got != 0.6 {
		t.Errorf("after a refused reload HeadroomTarget() = %v, want the running 0.6", got)
	}
}
