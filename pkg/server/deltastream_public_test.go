package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/monitor"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// collectFrames runs a DeltaStream and records what it emits.
type collectFrames struct {
	mu    sync.Mutex
	names []string
	sizes []int
	seqs  []uint64
}

func (c *collectFrames) emit(name string, payload []byte) {
	var envelope struct {
		Seq uint64 `json:"seq"`
	}
	_ = json.Unmarshal(payload, &envelope)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
	c.sizes = append(c.sizes, len(payload))
	c.seqs = append(c.seqs, envelope.Seq)
}

func (c *collectFrames) snapshotOf() ([]string, []int, []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...), append([]int(nil), c.sizes...), append([]uint64(nil), c.seqs...)
}

// changingSnapshotter returns a different snapshot each call, so the diff has
// something to find -- a stream of identical snapshots would produce empty
// patches and prove nothing about size.
type changingSnapshotter struct{ n atomic64 }

type atomic64 struct {
	mu sync.Mutex
	v  int
}

func (a *atomic64) next() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v++
	return a.v
}

func (s *changingSnapshotter) Snapshot() (*types.Snapshot, error) {
	n := s.n.next()
	procs := make([]types.ProcessDetail, 200)
	for i := range procs {
		procs[i] = types.ProcessDetail{PID: int32(i + 1), Name: "proc", CPUPercent: float64(i)}
	}
	// One field moves per tick; everything else is identical, which is what a
	// real machine between ticks looks like.
	procs[0].CPUPercent = float64(n)
	return &types.Snapshot{
		Timestamp: time.Now(),
		Host:      types.HostInfo{Hostname: "delta-host"},
		Processes: types.ProcessSummary{Total: len(procs), Processes: procs},
	}, nil
}

func quietStreamLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDeltaStream_SendsOneFullThenPatches is the contract the desktop bridge
// was missing: a client is given the whole snapshot once and patches after
// that, not the whole snapshot on every tick.
func TestDeltaStream_SendsOneFullThenPatches(t *testing.T) {
	mon, err := monitor.New(&changingSnapshotter{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	stream := NewDeltaStream(mon, quietStreamLogger())
	t.Cleanup(stream.Shutdown)

	frames := &collectFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Joined rather than abandoned: a Run still fanning out while the test
	// tears the monitor down is a goroutine writing into a fixture nobody is
	// reading, and the next test in the package inherits whatever it does.
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream.Run(ctx, frames.emit)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if names, _, _ := frames.snapshotOf(); len(names) >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	names, sizes, seqs := frames.snapshotOf()
	if len(names) < 4 {
		t.Fatalf("got %d frames in 3s, want at least 4", len(names))
	}
	if names[0] != "full" {
		t.Errorf("first frame is %q, want full -- a client cannot apply a patch to nothing", names[0])
	}
	for i, name := range names[1:] {
		if name != "delta" {
			t.Errorf("frame %d is %q, want delta; the whole snapshot is being resent", i+1, name)
		}
	}

	// Sequence numbers must be contiguous, or the client resyncs every tick and
	// the patches buy nothing.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("sequence went %d -> %d at frame %d", seqs[i-1], seqs[i], i)
		}
	}

	// And the point of the exercise: a patch is a fraction of a snapshot.
	for i, size := range sizes[1:] {
		if size >= sizes[0] {
			t.Errorf("delta %d is %d bytes against a full frame of %d; it is not a patch", i+1, size, sizes[0])
		}
	}
}

// TestDeltaStream_StopsWithTheContext covers the lifecycle: a window that
// closes must not leave the hub diffing for nobody.
func TestDeltaStream_StopsWithTheContext(t *testing.T) {
	mon, err := monitor.New(&changingSnapshotter{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	if err := mon.Start(); err != nil {
		t.Fatalf("monitor.Start: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	stream := NewDeltaStream(mon, quietStreamLogger())
	t.Cleanup(stream.Shutdown)

	frames := &collectFrames{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		stream.Run(ctx, frames.emit)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if names, _, _ := frames.snapshotOf(); len(names) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	before, _, _ := frames.snapshotOf()
	time.Sleep(100 * time.Millisecond)
	after, _, _ := frames.snapshotOf()
	if len(after) != len(before) {
		t.Errorf("frames kept arriving after cancellation: %d -> %d", len(before), len(after))
	}
}

// TestDeltaStream_NilEmitIsRefused is the error path: a caller with no sink
// would otherwise subscribe, hold the hub's publishing loop open and throw
// every frame away.
func TestDeltaStream_NilEmitIsRefused(t *testing.T) {
	mon, err := monitor.New(&changingSnapshotter{}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("monitor.New: %v", err)
	}
	stream := NewDeltaStream(mon, quietStreamLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		stream.Run(context.Background(), nil)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with a nil emit did not return immediately")
	}
}
