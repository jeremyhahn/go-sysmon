package server_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/pkg/delta"
	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/server"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// movingSnapshotter changes one field per collection, so consecutive snapshots
// differ in a way a patch has to carry.
type movingSnapshotter struct {
	uptime atomic.Uint64
}

func (s *movingSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := s.uptime.Add(1)
	return &types.Snapshot{
		Host: types.HostInfo{Hostname: "mon0", Uptime: n},
		CPUs: []types.CPUInfo{
			{Index: 0, ModelName: "Ryzen", UsagePercent: float64(n % 100)},
			{Index: 1, ModelName: "Ryzen", UsagePercent: float64((n * 7) % 100)},
		},
		Disks:    []types.DiskInfo{{Name: "sda", TotalBytes: 1 << 40, UsedPercent: 40}},
		Networks: []types.NetworkInfo{{Name: "eno1", IsUp: true, BytesRecvRate: float64(n)}},
	}, nil
}

func streamingServer(t *testing.T) *httptest.Server {
	t.Helper()
	m, err := monitor.New(&movingSnapshotter{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	stopMonitorAtCleanup(t, m)
	waitForSnapshot(t, m)

	srv := httptest.NewServer(server.New(m, "", nil))
	t.Cleanup(srv.Close)
	return srv
}

// fullFrame and deltaFrame are the two envelopes of the delta protocol.
type fullFrame struct {
	Seq      uint64          `json:"seq"`
	Snapshot json.RawMessage `json:"snapshot"`
}

type deltaFrame struct {
	Seq   uint64       `json:"seq"`
	Patch *delta.Patch `json:"patch"`
}

// TestEvents_LegacyStreamIsUnchanged.
//
// The event stream is a documented API, and this repository's own fleet poller
// is not its only possible client. Adding a protocol must not change the one
// that was there: no sequence numbers, no envelope, event still named
// "snapshot", payload still the bare snapshot.
func TestEvents_LegacyStreamIsUnchanged(t *testing.T) {
	srv := streamingServer(t)
	c := dialEvents(t, srv.URL+"/api/events", false)

	ev := c.nextNamed(t, "snapshot")

	var snap types.Snapshot
	decodeSnapshot(t, ev, &snap)
	if snap.Host.Hostname != "mon0" {
		t.Fatalf("hostname = %q, want mon0", snap.Host.Hostname)
	}

	// An envelope would parse into a Snapshot as an empty one, so the absence
	// of the wrapper is checked in the bytes.
	if strings.Contains(ev.Data, `"seq"`) {
		t.Fatalf("the legacy stream now carries a sequence number: %s", ev.Data)
	}
	for _, name := range []string{"full", "delta"} {
		if ev.Name == name {
			t.Fatalf("the legacy stream sent a %q event", name)
		}
	}
}

// TestEvents_DeltaStreamReconstructsTheSnapshot is the end-to-end contract:
// a client that applies what it is sent ends up holding what the server holds.
func TestEvents_DeltaStreamReconstructsTheSnapshot(t *testing.T) {
	srv := streamingServer(t)
	c := dialEvents(t, srv.URL+"/api/events?deltas=1", false)

	first := c.nextNamed(t, "full")
	var full fullFrame
	decodeSnapshot(t, first, &full)
	if full.Seq == 0 {
		t.Fatal("the first frame carries no sequence number")
	}

	state, err := delta.Decode(full.Snapshot)
	if err != nil {
		t.Fatalf("decoding the first snapshot: %v", err)
	}
	last := full.Seq

	// Apply the next five deltas, and whatever resyncs arrive among them,
	// then check the state they build.
	// Each frame arrives within the bound, so the loop needs one of its own: a
	// stream that sent nothing but full frames would otherwise run forever.
	const want = 5
	bound := time.NewTimer(await.Timeout)
	defer bound.Stop()
	applied := 0
	for applied < want {
		select {
		case <-bound.C:
			t.Fatalf("%d of %d delta frames arrived within %v; the stream is sending whole snapshots",
				applied, want, await.Timeout)
		default:
		}
		ev := c.nextEvent(t)
		switch ev.Name {
		case "delta":
			var frame deltaFrame
			decodeSnapshot(t, ev, &frame)
			if frame.Seq != last+1 {
				t.Fatalf("delta seq = %d, want %d; the server sent a patch across a gap",
					frame.Seq, last+1)
			}
			state, err = delta.Apply(state, frame.Patch)
			if err != nil {
				t.Fatalf("applying delta %d: %v", frame.Seq, err)
			}
			last = frame.Seq
			applied++
		case "full":
			var frame fullFrame
			decodeSnapshot(t, ev, &frame)
			state, err = delta.Decode(frame.Snapshot)
			if err != nil {
				t.Fatalf("decoding a resync: %v", err)
			}
			last = frame.Seq
		}
	}
	// The applied state has to be a well-formed snapshot, not merely a tree
	// that survived the arithmetic.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("re-encoding the applied state: %v", err)
	}
	var rebuilt types.Snapshot
	if err := json.Unmarshal(raw, &rebuilt); err != nil {
		t.Fatalf("the applied state is not a snapshot: %v", err)
	}
	if rebuilt.Host.Hostname != "mon0" || len(rebuilt.CPUs) != 2 || len(rebuilt.Disks) != 1 {
		t.Fatalf("the applied state lost structure: %+v", rebuilt.Host)
	}
	// The uptime advances once per collection, so a patched-in value proves the
	// deltas carried real change rather than being empty.
	if rebuilt.Host.Uptime <= 1 {
		t.Fatalf("uptime = %d after %d deltas; the patches carried nothing",
			rebuilt.Host.Uptime, applied)
	}
}

// TestEvents_DeltaStreamStartsEveryClientWithASnapshot.
//
// A client joining mid-stream has no base to patch. If it were sent a delta it
// would either fail or, worse, apply it to whatever it had -- which is the
// stale-reading failure this protocol exists to make impossible.
func TestEvents_DeltaStreamStartsEveryClientWithASnapshot(t *testing.T) {
	srv := streamingServer(t)

	first := dialEvents(t, srv.URL+"/api/events?deltas=1", false)
	first.nextNamed(t, "delta") // the stream is well underway

	second := dialEvents(t, srv.URL+"/api/events?deltas=1", false)
	ev, err := second.next()
	if err != nil {
		t.Fatalf("reading the second stream: %v", err)
	}
	for ev.Comment != "" || ev.Name == "" {
		if ev, err = second.next(); err != nil {
			t.Fatalf("reading the second stream: %v", err)
		}
	}
	if ev.Name != "full" {
		t.Fatalf("a client joining mid-stream was sent %q first, want full", ev.Name)
	}

	var frame fullFrame
	decodeSnapshot(t, ev, &frame)
	if frame.Seq == 0 || len(frame.Snapshot) == 0 {
		t.Fatalf("the joining client's first frame is empty: %+v", frame)
	}
}

// TestEvents_DeltaStreamCompresses. Compression and the delta protocol are
// independent, and each is worth having without the other; a client asking for
// both must get both.
func TestEvents_DeltaStreamCompresses(t *testing.T) {
	srv := streamingServer(t)
	c := dialEvents(t, srv.URL+"/api/events?deltas=1", true)

	if got := c.resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// The client transparently inflates, so reading a frame back proves the
	// stream is both compressed and readable.
	c.nextNamed(t, "full")
	c.nextNamed(t, "delta")
}

// TestEvents_DeltaOptInSpellings. The parameter is the only way a client asks
// for the new protocol, so the spellings it accepts are part of the API.
func TestEvents_DeltaOptInSpellings(t *testing.T) {
	srv := streamingServer(t)

	for _, query := range []string{"?deltas=1", "?deltas=true", "?deltas=yes"} {
		c := dialEvents(t, srv.URL+"/api/events"+query, false)
		c.nextNamed(t, "full")
	}
	for _, query := range []string{"", "?deltas=0", "?deltas=", "?deltas=no", "?delta=1"} {
		c := dialEvents(t, srv.URL+"/api/events"+query, false)
		c.nextNamed(t, "snapshot")
	}
}
