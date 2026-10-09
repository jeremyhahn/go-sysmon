//go:build integration

package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// The watcher's log lines are its observable decisions; the tests wait on them
// rather than on the passage of time.
const (
	msgHeld     = "config: file is being written, reload waits until it is closed"
	msgReloaded = "config: reloaded from disk"
	msgKept     = "config: reload failed, keeping previous settings"
	msgRejected = "config: reload applied, but some settings were rejected"
	msgDirGone  = "config: the config directory is gone, changes are no longer watched"
	msgStopped  = "config: stopped watching the config file"
)

// Two configurations that differ from each other and from the defaults (90).
const (
	startingConfig = "alerts:\n  cpu_pct: 42\n  memory_pct: 43\n"
	newConfig      = "alerts:\n  cpu_pct: 70\n  memory_pct: 71\n"
	firstLine      = "alerts:\n  cpu_pct: 70\n"
	restOfConfig   = "  memory_pct: 71\n"
)

// records hands every log record to the test, in order.
type records struct{ ch chan slog.Record }

func (h records) Enabled(context.Context, slog.Level) bool { return true }
func (h records) Handle(_ context.Context, r slog.Record) error {
	h.ch <- r.Clone()
	return nil
}
func (h records) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h records) WithGroup(string) slog.Handler      { return h }

// cpuMemory is the pair of settings the tests follow through each reload.
type cpuMemory struct{ cpu, memory float64 }

// writeFile writes body to path, creating it.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// replaceFile writes body beside path and renames it over path, as
// configuration management does.
func replaceFile(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".new"
	writeFile(t, tmp, body)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// watching is a Manager with its file watcher running.
type watching struct {
	mgr  *config.Manager
	log  chan slog.Record
	done chan struct{}
	over bool
}

// watch loads path, applies appliers to every reload and watches the file
// until the test ends.
func watch(t *testing.T, path string, appliers ...config.Applier) *watching {
	t.Helper()
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, a := range appliers {
		mgr.OnReload(a)
	}
	w := &watching{mgr: mgr, log: make(chan slog.Record, 256), done: make(chan struct{})}
	if err := mgr.Watch(w.done, slog.New(records{ch: w.log}), nil); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() {
		if !w.over {
			close(w.done)
		}
	})
	return w
}

// await returns the first record with message msg, skipping the others.
func (w *watching) await(t *testing.T, msg string) slog.Record {
	t.Helper()
	for {
		if r := await.Recv(t, "the watcher to log "+msg, w.log); r.Message == msg {
			return r
		}
	}
}

// awaitLive waits until the configuration in force carries want.
func (w *watching) awaitLive(t *testing.T, want cpuMemory) {
	t.Helper()
	await.Until(t, "the configuration in force to carry the new alert thresholds", func(context.Context) bool {
		return w.live() == want
	})
}

// live returns the thresholds in force.
func (w *watching) live() cpuMemory {
	a := w.mgr.Current().Alerts
	return cpuMemory{a.CPUPct, a.MemoryPct}
}

// errorAttr returns the error a record carries under "error".
func errorAttr(r slog.Record) error {
	var found error
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && a.Key == "error" {
			found = err
			return false
		}
		return true
	})
	return found
}

// TestConfigWatch_InPlaceWriteIsHeldUntilTheWriterCloses is a tool that
// truncates the file and writes it in place, stalling between writes: the half
// written file is never the configuration in force.
func TestConfigWatch_InPlaceWriteIsHeldUntilTheWriterCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(firstLine); err != nil {
		t.Fatal(err)
	}
	w.await(t, msgHeld)
	if got := w.live(); got != (cpuMemory{42, 43}) {
		t.Fatalf("with the file half written Current() = %v, want the previous {42 43}", got)
	}

	if _, err := f.WriteString(restOfConfig); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	w.await(t, msgReloaded)
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigWatch_AnEmptiedFileIsRefusedAndTheLastGoodConfigKept is a writer
// that truncates and closes before writing: loading nothing would silently drop
// every threshold, so the watcher warns and keeps what is running.
func TestConfigWatch_AnEmptiedFileIsRefusedAndTheLastGoodConfigKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	writeFile(t, path, "")
	r := w.await(t, msgKept)
	var empty *config.ConfigEmptyError
	if !errors.As(errorAttr(r), &empty) {
		t.Fatalf("the refusal carries %v, want a ConfigEmptyError", errorAttr(r))
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("the refusal was logged at %s, want WARN", r.Level)
	}
	if got := w.live(); got != (cpuMemory{42, 43}) {
		t.Errorf("after an empty file Current() = %v, want the last good {42 43}", got)
	}

	writeFile(t, path, newConfig)
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigWatch_AMalformedFileIsRefusedAndTheLastGoodConfigKept is a typo in
// the file: the reload fails and the running configuration stands.
func TestConfigWatch_AMalformedFileIsRefusedAndTheLastGoodConfigKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	replaceFile(t, path, "alerts: [this is not: a mapping\n")
	r := w.await(t, msgKept)
	if errorAttr(r) == nil {
		t.Error("the refusal of a malformed file carries no error")
	}
	if got := w.live(); got != (cpuMemory{42, 43}) {
		t.Errorf("after a malformed file Current() = %v, want the last good {42 43}", got)
	}
}

// TestConfigWatch_RemovedFileMeansDefaults is a file deleted: with no file the
// service runs on defaults, as it would have started, and a file written again
// is picked up.
func TestConfigWatch_RemovedFileMeansDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.awaitLive(t, cpuMemory{90, 90})

	replaceFile(t, path, newConfig)
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigWatch_UnrelatedFilesInTheDirectoryAreIgnored writes a sibling and
// the config in turn: only the config causes a reload.
func TestConfigWatch_UnrelatedFilesInTheDirectoryAreIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	writeFile(t, filepath.Join(dir, ".sysmon.yaml.swp"), "editor state")
	replaceFile(t, path, newConfig)
	r := w.await(t, msgReloaded)
	if r.Level != slog.LevelInfo {
		t.Errorf("the reload was logged at %s, want INFO", r.Level)
	}
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigWatch_ARelinkedConfigIsFollowed is the layout Kubernetes gives a
// mounted ConfigMap: the file is a link into a link to a versioned directory,
// and an update retargets the middle link.
func TestConfigWatch_ARelinkedConfigIsFollowed(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []struct{ name, body string }{{"v1", startingConfig}, {"v2", newConfig}} {
		if err := os.Mkdir(filepath.Join(dir, v.name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, v.name, "sysmon.yaml"), v.body)
	}
	if err := os.Symlink("v1", filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sysmon.yaml")
	if err := os.Symlink(filepath.Join("data", "sysmon.yaml"), path); err != nil {
		t.Fatal(err)
	}
	w := watch(t, path)
	if got := w.live(); got != (cpuMemory{42, 43}) {
		t.Fatalf("the config behind the links loaded as %v, want {42 43}", got)
	}

	next := filepath.Join(dir, "data.next")
	if err := os.Symlink("v2", next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	w.awaitLive(t, cpuMemory{70, 71})

	// The watcher follows the new target: a file replaced there is seen too.
	replaceFile(t, filepath.Join(dir, "v2", "sysmon.yaml"), startingConfig)
	w.awaitLive(t, cpuMemory{42, 43})
}

// TestConfigWatch_ALinkIntoADirectoryThatDoesNotExistYet is a config link whose
// target directory is created later, as a deployment tool does: the watcher
// picks the file up once the directory and the file appear.
func TestConfigWatch_ALinkIntoADirectoryThatDoesNotExistYet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysmon.yaml")
	if err := os.Symlink(filepath.Join("later", "sysmon.yaml"), path); err != nil {
		t.Fatal(err)
	}
	w := watch(t, path)
	if got := w.live(); got != (cpuMemory{90, 90}) {
		t.Fatalf("a dangling config link loaded as %v, want the defaults {90 90}", got)
	}

	staged := filepath.Join(dir, "staged.yaml")
	writeFile(t, staged, newConfig)
	if err := os.Mkdir(filepath.Join(dir, "later"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, filepath.Join(dir, "later", "sysmon.yaml")); err != nil {
		t.Fatal(err)
	}
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigWatch_DirectoryRemovedStopsTheWatcher is the directory itself
// going away: the watcher says so and releases its resources.
func TestConfigWatch_DirectoryRemovedStopsTheWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if r := w.await(t, msgDirGone); r.Level != slog.LevelWarn {
		t.Errorf("the missing directory was logged at %s, want WARN", r.Level)
	}
	w.await(t, msgStopped)
	w.over = true
}

// TestConfigWatch_RefusesADirectoryThatIsNotThere is Watch on a config whose
// directory does not exist: a typed error naming the directory.
func TestConfigWatch_RefusesADirectoryThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	mgr, err := config.NewManager(config.NewLoader(filepath.Join(missing, "sysmon.yaml")))
	if err != nil {
		t.Fatalf("NewManager on an absent file: %v", err)
	}
	var watchErr *config.ConfigWatchError
	if err := mgr.Watch(make(chan struct{}), slog.New(slog.DiscardHandler), nil); !errors.As(err, &watchErr) {
		t.Fatalf("Watch = %v, want a ConfigWatchError", err)
	} else if watchErr.Dir != missing {
		t.Errorf("ConfigWatchError.Dir = %q, want %q", watchErr.Dir, missing)
	}
}

// TestConfigWatch_ARejectingApplierIsReportedButTheReloadStands is a component
// that cannot take a new setting: the configuration is published for everything
// else and the watcher warns which component refused.
func TestConfigWatch_ARejectingApplierIsReportedButTheReloadStands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	w := watch(t, path, func(*config.Config) error { return errors.New("the interval is out of range") })

	replaceFile(t, path, newConfig)
	r := w.await(t, msgRejected)
	var applyErr *config.ApplyError
	if !errors.As(errorAttr(r), &applyErr) || !slices.Contains(applyErr.Reasons, "the interval is out of range") {
		t.Errorf("the warning carries %v, want an ApplyError naming the refusal", errorAttr(r))
	}
	w.awaitLive(t, cpuMemory{70, 71})
}

// TestConfigReload_ReportsWhatTheApplierRefused is the same through Reload, as
// SIGHUP and the HTTP endpoint call it.
func TestConfigReload_ReportsWhatTheApplierRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, startingConfig)
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	mgr.OnReload(func(*config.Config) error { return errors.New("first refusal") })
	mgr.OnReload(func(*config.Config) error { return errors.New("second refusal") })

	writeFile(t, path, newConfig)
	_, err = mgr.Reload()
	var applyErr *config.ApplyError
	if !errors.As(err, &applyErr) {
		t.Fatalf("Reload = %v, want an ApplyError", err)
	}
	if !strings.Contains(err.Error(), "first refusal; second refusal") {
		t.Errorf("the error %q does not list both refusals", err)
	}
	if got := mgr.Current().Alerts.CPUPct; got != 70 {
		t.Errorf("cpu_pct = %v after a reload an applier refused, want it published: 70", got)
	}
}

// TestConfig_OutOfRangeHeadroomTargetIsRefused is a value the operator wrote
// and cannot mean: refused at startup, and on reload with the old one kept.
func TestConfig_OutOfRangeHeadroomTargetIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, "fleet:\n  headroom_target: 2\n")

	var rangeErr *config.HeadroomTargetError
	if _, err := config.NewManager(config.NewLoader(path)); !errors.As(err, &rangeErr) {
		t.Fatalf("NewManager = %v, want a HeadroomTargetError", err)
	} else if !strings.Contains(err.Error(), "fleet.headroom_target 2 is out of range") {
		t.Errorf("the error %q does not name the setting and value", err)
	}

	writeFile(t, path, "fleet:\n  headroom_target: 0.5\n")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "fleet:\n  headroom_target: -1\n")
	if _, err := mgr.Reload(); !errors.As(err, &rangeErr) {
		t.Fatalf("Reload = %v, want a HeadroomTargetError", err)
	}
	if got := mgr.Current().Fleet.HeadroomTarget(); got != 0.5 {
		t.Errorf("the headroom target is %v after a refused reload, want the previous 0.5", got)
	}
}

// TestConfig_ABlankListenAddressFallsBackToTheDefault is an addr the file
// names and leaves empty: the default is used and attributed to the defaults,
// not to the file.
func TestConfig_ABlankListenAddressFallsBackToTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeFile(t, path, "server:\n  addr: \"\"\n")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mgr.Current().Server.Addr, config.Defaults().Server.Addr; got != want {
		t.Errorf("server.addr = %q, want the default %q", got, want)
	}
	if got := mgr.Sources()["server.addr"]; got != types.ConfigSourceDefault {
		t.Errorf("server.addr is attributed to %q, want the defaults", got)
	}
}

// TestConfig_HistoryCadenceMustFitTheDatabaseLookback checks the validator the
// serve command registers: a node that writes history too slowly for the
// database to chart it is refused, for either of the intervals that pace it.
func TestConfig_HistoryCadenceMustFitTheDatabaseLookback(t *testing.T) {
	const lookback = 5 * time.Minute
	lookbackMs := int(lookback.Milliseconds())

	cfg := config.Defaults()
	if err := config.CheckHistoryCadence(&cfg, lookback); err != nil {
		t.Fatalf("a node that writes no history was refused: %v", err)
	}

	cfg.Prometheus.RemoteWriteURL = "http://mon0:9090/api/v1/write"
	cfg.Prometheus.SampleIntervalMs = 10 * lookbackMs
	var cadence *config.HistoryCadenceError
	if err := config.CheckHistoryCadence(&cfg, lookback); !errors.As(err, &cadence) {
		t.Fatalf("a sample interval of %dms against a lookback of %dms = %v, want a HistoryCadenceError", cfg.Prometheus.SampleIntervalMs, lookbackMs, err)
	} else if cadence.LookbackMs != lookbackMs {
		t.Errorf("HistoryCadenceError.LookbackMs = %d, want %d", cadence.LookbackMs, lookbackMs)
	}

	cfg.Prometheus.SampleIntervalMs = 15_000
	cfg.Fleet.Peers = []config.Peer{{Name: "a", URL: "http://a:8080"}}
	cfg.Fleet.PollIntervalMs = 15_000
	cfg.Fleet.TimeoutMs = 120_000
	if err := config.CheckHistoryCadence(&cfg, lookback); !errors.As(err, &cadence) {
		t.Fatalf("a peer timeout of %dms against a lookback of %dms = %v, want a HistoryCadenceError", cfg.Fleet.TimeoutMs, lookbackMs, err)
	} else if !strings.Contains(cadence.Key, "fleet.poll_interval_ms") {
		t.Errorf("HistoryCadenceError.Key = %q, want the fleet cadence", cadence.Key)
	}
}

// TestConfig_ReservedLabelNamesAreListedInOrder is the list the settings
// documentation and the label check share.
func TestConfig_ReservedLabelNamesAreListedInOrder(t *testing.T) {
	names := config.ReservedLabelNames()
	if !sort.StringsAreSorted(names) {
		t.Errorf("ReservedLabelNames() = %v, want sorted", names)
	}
	for _, want := range []string{"severity", "source", "resource"} {
		if !slices.Contains(names, want) {
			t.Errorf("ReservedLabelNames() = %v, missing %q", names, want)
		}
	}
}
