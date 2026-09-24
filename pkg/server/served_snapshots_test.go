package server_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// alertingSnapshotter returns the same reading every time, with a disk full
// enough to raise an alert under the default thresholds. Constant, timestamp
// included, so every snapshot it produces decorates to the same JSON and any
// difference between two serving paths is a difference in what the path
// attached -- not a collection that happened between two requests.
type alertingSnapshotter struct{}

// alertingTakenAt is the fixed time every alertingSnapshotter reading carries.
var alertingTakenAt = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

func (alertingSnapshotter) Snapshot() (*types.Snapshot, error) {
	return &types.Snapshot{
		Timestamp: alertingTakenAt,
		Host:      types.HostInfo{Hostname: "host"},
		Disks:     []types.DiskInfo{{Name: "sda", TotalBytes: 1 << 40, UsedPercent: 97}},
	}, nil
}

// startedMonitor runs an alertingSnapshotter monitor until the test ends.
func startedMonitor(t *testing.T) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(alertingSnapshotter{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Stop(); err != nil {
			t.Errorf("monitor.Stop: %v", err)
		}
	})
	return m
}

// getJSON fetches url and decodes the body into v.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("GET %s: close body: %v", url, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
}

// servedSnapshots collects this node's snapshot from every path that serves
// one, each decoded to a generic map so a field one path carries and another
// lacks shows up as a difference rather than being zeroed by a struct. self,
// when set, adds the fleet's view of this node.
func servedSnapshots(t *testing.T, base, self string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}

	var polled map[string]any
	getJSON(t, base+"/api/snapshot", &polled)
	out["GET /api/snapshot"] = polled

	var legacy map[string]any
	decodeSnapshot(t, dialEvents(t, base+"/api/events", false).nextNamed(t, "snapshot"), &legacy)
	out["GET /api/events (snapshot frame)"] = legacy

	var full fullFrame
	decodeSnapshot(t, dialEvents(t, base+"/api/events?deltas=1", false).nextNamed(t, "full"), &full)
	var streamed map[string]any
	if err := json.Unmarshal(full.Snapshot, &streamed); err != nil {
		t.Fatalf("full frame snapshot: %v", err)
	}
	out["GET /api/events?deltas=1 (full frame)"] = streamed

	if self != "" {
		var peer struct {
			Snapshot map[string]any `json:"snapshot"`
		}
		getJSON(t, base+"/api/fleet/"+self+"/snapshot", &peer)
		out["GET /api/fleet/"+self+"/snapshot"] = peer.Snapshot
	}
	return out
}

// assertIdentical fails for every field in which a path's snapshot differs
// from the polled one, and returns the polled one.
func assertIdentical(t *testing.T, served map[string]map[string]any) map[string]any {
	t.Helper()
	want := served["GET /api/snapshot"]
	for path, got := range served {
		keys := map[string]struct{}{}
		for k := range want {
			keys[k] = struct{}{}
		}
		for k := range got {
			keys[k] = struct{}{}
		}
		for key := range keys {
			if !reflect.DeepEqual(want[key], got[key]) {
				t.Errorf("%s: field %q = %v, /api/snapshot has %v", path, key, got[key], want[key])
			}
		}
	}
	return want
}

// hasAlerts reports whether a decoded snapshot carries at least one alert.
func hasAlerts(snap map[string]any) bool {
	alerts, ok := snap["alerts"].([]any)
	return ok && len(alerts) > 0
}

// TestServedSnapshots_EmbeddedServerCarriesEveryField is the embedded shape:
// server.New with nothing but a monitor. Its stream used to lack the build,
// the node's name and the alerts, because only the CLI's own snapshotter
// attached them. Every path must now serve the same, complete snapshot.
func TestServedSnapshots_EmbeddedServerCarriesEveryField(t *testing.T) {
	m := startedMonitor(t)
	waitForSnapshot(t, m)
	ts := httptest.NewServer(server.New(m, ":0", nil))
	t.Cleanup(ts.Close)

	snap := assertIdentical(t, servedSnapshots(t, ts.URL, ""))

	if want := (&config.Config{}).LocalName(); snap["node_name"] != want {
		t.Errorf("node_name = %v, want the resolved local name %q", snap["node_name"], want)
	}
	if !hasAlerts(snap) {
		t.Errorf("alerts = %v, want the disk alert the default thresholds raise", snap["alerts"])
	}
}

// writeConfig writes body as the configuration file at path.
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// awaitNodeName returns once m has published a snapshot naming want. The
// monitor stores a reading before it broadcasts it, so from then on every
// serving path reads a snapshot with that name.
func awaitNodeName(t *testing.T, m *monitor.Monitor, want string) {
	t.Helper()
	sub := m.Subscribe()
	defer m.Unsubscribe(sub.ID)
	if snap, err := m.Snapshot(); err == nil && snap.NodeName == want {
		return
	}
	for {
		if snap := await.Recv(t, "a snapshot naming "+want, sub.Ch); snap.NodeName == want {
			return
		}
	}
}

// TestServedSnapshots_ConfiguredServerAgreesAcrossARename is the CLI's shape:
// a configuration, a fleet and a build. Every path -- the fleet's view of this
// node included -- serves the same build, name and alerts, and a reload that
// renames the node reaches all of them without waiting for a collection.
func TestServedSnapshots_ConfiguredServerAgreesAcrossARename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysmon.yaml")
	writeConfig(t, path, "fleet:\n  local_name: db-01\n")
	mgr, err := config.NewManager(config.NewLoader(path))
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}

	m := startedMonitor(t)
	poller := fleet.New(
		func() config.Fleet { return mgr.Current().Fleet },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) {
			prev, cur, err := m.Consecutive()
			if err != nil {
				return nil, nil, 0
			}
			return prev, cur, m.Interval()
		},
		func() string { return mgr.Current().LocalName() },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv, err := server.NewWithConfig(server.Config{
		Monitor: m, Addr: ":0", Fleet: poller, Config: mgr,
		Version: "0.1.0-alpha", GitCommit: "a46f6cf", BuildDate: "2026-09-05T03:26:50Z",
	})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	awaitNodeName(t, m, "db-01")

	snap := assertIdentical(t, servedSnapshots(t, ts.URL, "db-01"))
	for field, want := range map[string]string{
		"version": "0.1.0-alpha", "git_commit": "a46f6cf",
		"build_date": "2026-09-05T03:26:50Z", "node_name": "db-01",
	} {
		if snap[field] != want {
			t.Errorf("%s = %v, want %q", field, snap[field], want)
		}
	}
	if !hasAlerts(snap) {
		t.Errorf("alerts = %v, want the disk alert", snap["alerts"])
	}

	// Slow the monitor right down, so the rename below reaches the served
	// snapshot by the restamp rather than by a new collection.
	if err := m.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	writeConfig(t, path, "fleet:\n  local_name: database\n")
	if _, err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// No waiting: the reload's restamp is published before Reload returns.
	var polled map[string]any
	getJSON(t, ts.URL+"/api/snapshot", &polled)
	if polled["node_name"] != "database" {
		t.Fatalf("node_name = %v straight after the reload, want database", polled["node_name"])
	}

	renamed := assertIdentical(t, servedSnapshots(t, ts.URL, "database"))
	if !reflect.DeepEqual(renamed["alerts"], snap["alerts"]) {
		t.Errorf("alerts changed on a rename: %v, were %v -- a restamp must not re-evaluate",
			renamed["alerts"], snap["alerts"])
	}
}

// TestNewWithConfig_SharesOrRefusesAMonitorsDecoration: one monitor describes
// one node. A second server describing it identically shares the first's
// decoration; one describing it differently is refused rather than silently
// overwriting the first's build, name and alert history; and server.New,
// which describes nothing of its own, serves the decoration already there.
func TestNewWithConfig_SharesOrRefusesAMonitorsDecoration(t *testing.T) {
	m := startedMonitor(t)
	waitForSnapshot(t, m)
	cfg := server.Config{Monitor: m, Addr: ":0", Version: "1.0.0", GitCommit: "a46f6cf"}

	if _, err := server.NewWithConfig(cfg); err != nil {
		t.Fatalf("first NewWithConfig: %v", err)
	}
	installed := m.Decorator()

	if _, err := server.NewWithConfig(cfg); err != nil {
		t.Errorf("identical second server: %v, want it to share", err)
	}
	if m.Decorator() != installed {
		t.Error("the identical second server replaced the decorator instead of sharing it")
	}

	other := cfg
	other.GitCommit = "73d0de5"
	var refused *types.DecoratorInstalledError
	if _, err := server.NewWithConfig(other); !errors.As(err, &refused) {
		t.Errorf("differing second server: %v, want DecoratorInstalledError", err)
	}

	ts := httptest.NewServer(server.New(m, ":0", nil))
	t.Cleanup(ts.Close)
	var snap map[string]any
	getJSON(t, ts.URL+"/api/snapshot", &snap)
	if snap["git_commit"] != "a46f6cf" {
		t.Errorf("server.New served git_commit %v, want the installed decoration's a46f6cf", snap["git_commit"])
	}
}
