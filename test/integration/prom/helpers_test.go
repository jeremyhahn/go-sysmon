//go:build integration

package prom

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/fleet"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// serveServer starts the server build with config.
func serveServer(t *testing.T, config string, env ...string) *harness.Server {
	t.Helper()
	return harness.Serve(t, harness.ServeOptions{Bin: harness.Bins.Server, Config: config, Env: env})
}

// serveServerAt starts the server build reading the config file at path, for
// a test that rewrites it and reloads.
func serveServerAt(t *testing.T, path string, env ...string) *harness.Server {
	t.Helper()
	return harness.Serve(t, harness.ServeOptions{Bin: harness.Bins.Server, ConfigPath: path, Env: env})
}

// configFile writes body as a config file and returns its path.
func configFile(t *testing.T, body string) string {
	t.Helper()
	return harness.WriteFile(t, "sysmon.yaml", body)
}

// reloadOutcomes are the two lines a SIGHUP reload ends in: exactly one of
// them is logged per signal.
var reloadOutcomes = map[string]bool{
	"config: reloaded on SIGHUP":                              true,
	"config: SIGHUP reload failed, keeping previous settings": true,
}

// node is a running server and the config file it reads.
type node struct {
	*harness.Server
	path    string
	reloads int
}

// serveNode starts the server build reading a config file holding body.
func serveNode(t *testing.T, body string, env ...string) *node {
	t.Helper()
	path := configFile(t, body)
	return &node{Server: serveServerAt(t, path, env...), path: path}
}

// reload rewrites the node's config and sends SIGHUP, returning the log line
// the reload ended in.
func (n *node) reload(t *testing.T, body string) harness.LogRecord {
	t.Helper()
	if err := os.WriteFile(n.path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", n.path, err)
	}
	n.reloads++
	want := n.reloads
	seen := 0
	n.Signal(t, syscall.SIGHUP)
	return n.Logs.Await(t, "SIGHUP reload "+strconv.Itoa(want)+" to finish", func(r harness.LogRecord) bool {
		if reloadOutcomes[r.Msg()] {
			seen++
		}
		return seen == want
	})
}

// scrape fetches /metrics and returns its body, requiring the exposition
// content type.
func scrape(t *testing.T, s *harness.Server) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL("/metrics"), nil)
	if err != nil {
		t.Fatal(err)
	}
	status, header, body := harness.Do(t, http.DefaultClient, req)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", status, body)
	}
	if ct := header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("GET /metrics Content-Type = %q, want the text exposition format", ct)
	}
	return string(body)
}

// metricValue returns the value of the sample line name{labels} in an
// exposition, labels given as they are written, e.g. `severity="warning"`.
func metricValue(exposition, name string, labels ...string) (float64, bool) {
	prefix := name
	if len(labels) > 0 {
		prefix += "{" + strings.Join(labels, ",") + "}"
	}
	scanner := bufio.NewScanner(strings.NewReader(exposition))
	for scanner.Scan() {
		line := scanner.Text()
		rest, ok := strings.CutPrefix(line, prefix+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// awaitScrape scrapes /metrics until cond accepts the exposition, failing
// the test if it has not within await's bound. The exporter's state has no
// notification outside the process, so polling the endpoint is the only way
// to see it.
func awaitScrape(t *testing.T, s *harness.Server, what string, cond func(exposition string) bool) string {
	t.Helper()
	var last string
	await.Until(t, what, func(context.Context) bool {
		last = scrape(t, s)
		return cond(last)
	})
	return last
}

// snapshotAlerts returns the alerts in the server's current snapshot.
func snapshotAlerts(t *testing.T, s *harness.Server) []types.Alert {
	t.Helper()
	var snap struct {
		Alerts []types.Alert `json:"alerts"`
	}
	harness.GetJSON(t, s.URL("/api/snapshot"), &snap)
	return snap.Alerts
}

// hasAlert reports whether alerts holds one for rule.
func hasAlert(alerts []types.Alert, rule string) (types.Alert, bool) {
	for _, a := range alerts {
		if a.Rule == rule {
			return a, true
		}
	}
	return types.Alert{}, false
}

// ── systemd notification ─────────────────────────────────────────────────────

// notifySocket is a stand-in for systemd's notification socket: the path to
// pass as NOTIFY_SOCKET, and every datagram the service sends to it.
type notifySocket struct {
	path string
	mu   sync.Mutex
	msgs []string
	ch   chan struct{}
}

// newNotifySocket listens on a unixgram socket in a temporary directory.
func newNotifySocket(t *testing.T) *notifySocket {
	t.Helper()
	n := &notifySocket{path: filepath.Join(t.TempDir(), "notify.sock"), ch: make(chan struct{})}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: n.path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on %s: %v", n.path, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.msgs = append(n.msgs, string(buf[:k]))
			close(n.ch)
			n.ch = make(chan struct{})
			n.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close the notification socket: %v", err)
		}
		<-done
	})
	return n
}

// env is the variable pointing the service at this socket.
func (n *notifySocket) env() string { return "NOTIFY_SOCKET=" + n.path }

// awaitStatus returns the first STATUS= line match accepts, failing the test
// if none has arrived within await.Timeout of being called.
func (n *notifySocket) awaitStatus(t *testing.T, what string, match func(status string) bool) string {
	t.Helper()
	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	for i := 0; ; {
		n.mu.Lock()
		msgs, ch := n.msgs, n.ch
		n.mu.Unlock()
		for ; i < len(msgs); i++ {
			if status, ok := strings.CutPrefix(msgs[i], "STATUS="); ok && match(status) {
				return status
			}
		}
		select {
		case <-ch:
		case <-bound.C:
			t.Fatalf("await: %s did not happen within %v; the service sent %q", what, await.Timeout, msgs)
		}
	}
}

// ── A stand-in peer ──────────────────────────────────────────────────────────

// peerNode is a stand-in for a go-sysmon node the aggregator polls. It
// answers the fleet's summary and history endpoints with a snapshot the test
// controls, so the aggregator records readings of hardware the devcontainer
// does not have, at clock times the test chooses.
type peerNode struct {
	srv *httptest.Server

	mu   sync.Mutex
	snap types.Snapshot
	// intervalMs is the collection interval the summary reports; 0 omits it,
	// as a build older than the fleet summary does.
	intervalMs int64
}

// newPeerNode serves snap under the name the snapshot carries.
func newPeerNode(t *testing.T, snap types.Snapshot, intervalMs int64) *peerNode {
	t.Helper()
	p := &peerNode{snap: snap, intervalMs: intervalMs}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		snap := p.snap
		summary := fleet.Summarize(nil, &snap)
		summary.CollectionIntervalMs = p.intervalMs
		p.mu.Unlock()
		writeEnvelope(w, http.StatusOK, summary)
	})
	serveSnap := func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		snap := p.snap
		p.mu.Unlock()
		writeEnvelope(w, http.StatusOK, snap)
	}
	mux.HandleFunc("GET /api/snapshot/history", serveSnap)
	mux.HandleFunc("GET /api/snapshot", serveSnap)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// setTimestamp dates the reading the peer serves.
func (p *peerNode) setTimestamp(ts time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snap.Timestamp = ts
}

// gpuBox is the snapshot of a node with the hardware the devcontainer lacks:
// a GPU, an NPU, package power, core temperatures, an NVMe drive with SMART,
// a RAID volume with drives behind its controller, a degraded md array, and a
// firing alert.
func gpuBox(name string, ts time.Time) types.Snapshot {
	return types.Snapshot{
		Timestamp: ts,
		NodeName:  name,
		Version:   "test",
		Host:      types.HostInfo{Hostname: name, Uptime: 7200},
		CPUs: []types.CPUInfo{
			{UsagePercent: 40, UsageAvailable: true},
			{UsagePercent: 60, UsageAvailable: true},
		},
		LoadAvg: types.LoadAverage{Load1: 1.5, Load5: 1.25, Load15: 1},
		Memory: types.MemoryInfo{
			TotalBytes: 64 << 30, UsedBytes: 16 << 30, UsedPercent: 25, SwapPercent: 1,
			PageSizeBytes: 4096, SwapCountersAvailable: true, SwapInPages: 10, SwapOutPages: 20,
		},
		GPUs: []types.GPUInfo{{Index: 0, GPUUtilPercent: 73, MemoryPercent: 40, TemperatureGPU: 66, PowerDrawW: 180}},
		NPUs: []types.NPUInfo{{Index: 0, PowerDrawW: 4, ResidencyPercent: 50, UtilPercent: 12, ClientCount: 2,
			TemperatureC: 45, FrequencyMHz: 1400, CountersStale: true, CountersAgeSec: 3}},
		Sensors: types.SensorData{
			CoreTemps:    []types.CoreTemp{{CoreID: 0, TempCelsius: 61}},
			PackagePower: []types.PackagePower{{PackageName: "package-0", PowerW: 55.5}},
			PSI: types.PSIData{Available: true, CPU: types.PSIResource{SomeAvg10: 2.5, FullAvg10: 0.5},
				Memory: types.PSIResource{SomeAvg10: 1}, IO: types.PSIResource{SomeAvg10: 3, FullAvg10: 1}},
		},
		Disks: []types.DiskInfo{
			{Name: "nvme0n1", UtilPercent: 5, TotalBytes: 1 << 40, UsedPercent: 30, Temperature: 41,
				SMARTEnabled: true, SMARTHealthy: true, PowerOnHours: 1000, WearLevelPercent: 3, AvailableSparePercent: 100},
			{Name: "sda", TotalBytes: 4 << 40, UsedPercent: 60, Members: []types.DiskMember{
				{Serial: "WD-0001", Temperature: 35, SMARTEnabled: true, SMARTHealthy: true},
				{Serial: ""},
			}},
			{Name: "md0", RAID: &types.RAIDInfo{Level: "raid1", Degraded: true, ActiveDevices: 1, TotalDevices: 2,
				SyncAction: "recover", SyncProgressPercent: 12.5}},
		},
		Networks: []types.NetworkInfo{
			{Name: "lo", IsLoopback: true, IsUp: true},
			{Name: "eth0", IsUp: true, BytesRecvRate: 1000, BytesSentRate: 500},
			{Name: "eth1", IsUp: false},
		},
		Alerts: []types.Alert{{ID: "gpu-box/raid", Rule: "raid_degraded", Severity: types.SeverityCritical,
			Category: "storage", Resource: "md0", Message: "md0 is degraded"}},
	}
}

// labelsOf is a label set for tsdb.matching.
func labelsOf(pairs ...string) map[string]string {
	out := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = pairs[i+1]
	}
	return out
}

// decodeJSON unmarshals body, failing the test on malformed JSON.
func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}
