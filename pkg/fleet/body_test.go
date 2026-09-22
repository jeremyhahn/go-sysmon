package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// encodeSnapshot renders a snapshot the way a peer's handler does.
func encodeSnapshot(t testing.TB, snap *types.Snapshot) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(snap); err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	return buf.Bytes()
}

// chunked delivers a body a few bytes at a time, the way a chunked HTTP
// response with no Content-Length arrives, so a buffer cannot be sized from
// the first read.
func chunked(body []byte) io.Reader {
	return iotest.HalfReader(bytes.NewReader(body))
}

// kept returns the buffer currently held for peer, or nil.
func (b *bodyBuffers) kept(peer string) *bytes.Buffer {
	v, ok := b.m.Load(peer)
	if !ok {
		return nil
	}
	return v.(*bytes.Buffer)
}

func TestBodyBuffers_DecodeReusesThePeersBuffer(t *testing.T) {
	body := encodeSnapshot(t, benchSnapshot("omega"))
	var b bodyBuffers

	var first types.Snapshot
	if err := b.decode("omega", chunked(body), maxSnapshotBytes, &first); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if first.Host.Hostname != "omega" || len(first.Processes.Processes) != benchProcesses {
		t.Fatalf("first decode = host %q with %d processes; want omega with %d",
			first.Host.Hostname, len(first.Processes.Processes), benchProcesses)
	}
	buf := b.kept("omega")
	if buf == nil {
		t.Fatal("no buffer kept for omega after a successful decode")
	}
	capacity := buf.Cap()

	var second types.Snapshot
	if err := b.decode("omega", chunked(body), maxSnapshotBytes, &second); err != nil {
		t.Fatalf("second decode: %v", err)
	}
	// The same buffer, not grown: the reuse is the whole point, and a buffer
	// that was replaced or regrown every cycle would pass every other test here.
	if got := b.kept("omega"); got != buf {
		t.Error("second decode of the same peer did not reuse its buffer")
	}
	if got := b.kept("omega").Cap(); got != capacity {
		t.Errorf("buffer grew from %d to %d bytes on a same-sized body", capacity, got)
	}
}

func TestBodyBuffers_DecodedValueDoesNotAliasTheBuffer(t *testing.T) {
	var b bodyBuffers

	var first types.Snapshot
	if err := b.decode("omega", chunked(encodeSnapshot(t, benchSnapshot("aaaaa"))), maxSnapshotBytes, &first); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	// Overwrite the same buffer with a body whose strings sit at the same
	// offsets. A decoded string backed by the buffer would now read "bbbbb".
	var second types.Snapshot
	if err := b.decode("omega", chunked(encodeSnapshot(t, benchSnapshot("bbbbb"))), maxSnapshotBytes, &second); err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if first.Host.Hostname != "aaaaa" {
		t.Errorf("first snapshot's hostname changed to %q after the buffer was reused; "+
			"the cached snapshot aliases the read buffer", first.Host.Hostname)
	}
	if second.Host.Hostname != "bbbbb" {
		t.Errorf("second snapshot's hostname = %q, want bbbbb", second.Host.Hostname)
	}
}

func TestBodyBuffers_PeersDoNotShareABuffer(t *testing.T) {
	var b bodyBuffers
	for _, peer := range []string{"omega", "gpu-host"} {
		var snap types.Snapshot
		if err := b.decode(peer, chunked(encodeSnapshot(t, benchSnapshot(peer))), maxSnapshotBytes, &snap); err != nil {
			t.Fatalf("decode %s: %v", peer, err)
		}
		if snap.Host.Hostname != peer {
			t.Errorf("decode %s returned hostname %q", peer, snap.Host.Hostname)
		}
	}
	if b.kept("omega") == nil || b.kept("gpu-host") == nil {
		t.Fatal("expected a buffer kept for each peer")
	}
	if b.kept("omega") == b.kept("gpu-host") {
		t.Error("two peers share one buffer")
	}
}

func TestBodyBuffers_DropsABufferFarLargerThanItsBody(t *testing.T) {
	var b bodyBuffers

	var big types.Snapshot
	if err := b.decode("omega", chunked(encodeSnapshot(t, benchSnapshot("omega"))), maxSnapshotBytes, &big); err != nil {
		t.Fatalf("decode large body: %v", err)
	}
	large := b.kept("omega")

	// The peer now answers with a snapshot a small fraction of the size -- its
	// processes are gone. Keeping the old buffer would pin its largest-ever
	// body for the life of the process.
	var small types.Snapshot
	if err := b.decode("omega", chunked(encodeSnapshot(t, healthySmall("omega"))), maxSnapshotBytes, &small); err != nil {
		t.Fatalf("decode small body: %v", err)
	}
	if got := b.kept("omega"); got == large {
		t.Errorf("kept a %d-byte buffer holding a %d-byte body", got.Cap(), got.Len())
	}
}

func TestBodyBuffers_DecodeErrors(t *testing.T) {
	valid := encodeSnapshot(t, benchSnapshot("omega"))
	readFailure := errors.New("connection reset")

	tests := []struct {
		name  string
		body  io.Reader
		limit int64
		want  error
	}{
		{name: "malformed JSON", body: strings.NewReader(`{"host": `), limit: maxSnapshotBytes},
		{name: "wrong type", body: strings.NewReader(`{"host": 42}`), limit: maxSnapshotBytes},
		{name: "empty body", body: strings.NewReader(``), limit: maxSnapshotBytes},
		// A body past the limit is cut off, so it cannot parse: an oversized
		// peer is refused rather than read without bound.
		{name: "body over the limit", body: bytes.NewReader(valid), limit: int64(len(valid) / 2)},
		{name: "read failure", body: iotest.ErrReader(readFailure), limit: maxSnapshotBytes, want: readFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bodyBuffers
			var snap types.Snapshot
			err := b.decode("omega", tt.body, tt.limit, &snap)
			if err == nil {
				t.Fatal("decode succeeded; want an error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("decode error = %v; want %v", err, tt.want)
			}
		})
	}
}

// TestBodyBuffers_ConcurrentDecodesOfOnePeer holds the ownership rule: a buffer
// is out of the map for as long as a decode is reading it, so two fetches of
// the same peer never write into one buffer. Run under -race, sharing would be
// reported; without it, a torn body would fail to parse or decode the wrong
// hostname.
func TestBodyBuffers_ConcurrentDecodesOfOnePeer(t *testing.T) {
	var b bodyBuffers
	hosts := []string{"aaaaa", "bbbbb", "ccccc", "ddddd"}
	bodies := make([][]byte, len(hosts))
	for i, h := range hosts {
		bodies[i] = encodeSnapshot(t, benchSnapshot(h))
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(hosts)*20)
	for i := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				var snap types.Snapshot
				if err := b.decode("omega", chunked(bodies[i]), maxSnapshotBytes, &snap); err != nil {
					errs <- err
					return
				}
				if snap.Host.Hostname != hosts[i] {
					errs <- &hostMismatch{got: snap.Host.Hostname, want: hosts[i]}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type hostMismatch struct{ got, want string }

func (e *hostMismatch) Error() string {
	return "decoded hostname " + e.got + ", want " + e.want + ": a buffer was shared between decodes"
}

func TestBodyBuffers_Forget(t *testing.T) {
	var b bodyBuffers
	for _, peer := range []string{"omega", "gpu-host"} {
		var snap types.Snapshot
		if err := b.decode(peer, chunked(encodeSnapshot(t, benchSnapshot(peer))), maxSnapshotBytes, &snap); err != nil {
			t.Fatalf("decode %s: %v", peer, err)
		}
	}

	b.forget(map[string]bool{"gpu-host": true})

	if b.kept("omega") != nil {
		t.Error("buffer for a removed peer was kept")
	}
	if b.kept("gpu-host") == nil {
		t.Error("buffer for a configured peer was dropped")
	}
}

// TestRefresh_ForgetsBuffersOfRemovedPeers is the wiring: a peer dropped by a
// configuration reload must not leave its buffer behind for the life of the
// process.
func TestRefresh_ForgetsBuffersOfRemovedPeers(t *testing.T) {
	p, cfg := benchFleet(t, 2)
	p.refresh(cfg)
	for _, peer := range cfg.Peers {
		if p.bodies.kept(peer.Name) == nil {
			t.Fatalf("no buffer kept for %s after a refresh", peer.Name)
		}
	}

	removed := cfg.Peers[1].Name
	cfg.Peers = cfg.Peers[:1]
	p.refresh(cfg)

	if p.bodies.kept(removed) != nil {
		t.Errorf("buffer for %s kept after it was removed from the configuration", removed)
	}
	if p.bodies.kept(cfg.Peers[0].Name) == nil {
		t.Errorf("buffer for %s dropped though it is still configured", cfg.Peers[0].Name)
	}
}

// TestCycle_ForgetsBuffersWhenTheLastPeerIsRemoved covers the reload refresh
// cannot: with no peers left, Run never calls refresh, so without this the
// buffers of every former peer would stay pinned for the life of the process.
func TestCycle_ForgetsBuffersWhenTheLastPeerIsRemoved(t *testing.T) {
	p, cfg := benchFleet(t, 2)
	p.cycle(cfg)
	for _, peer := range cfg.Peers {
		if p.bodies.kept(peer.Name) == nil {
			t.Fatalf("no buffer kept for %s after a cycle", peer.Name)
		}
	}

	cfg.Peers = nil
	p.cycle(cfg)

	p.bodies.m.Range(func(k, _ any) bool {
		t.Errorf("buffer for %v kept after every peer was removed", k)
		return true
	})
}

// healthySmall is a snapshot with no process list, a small fraction of the
// size of benchSnapshot.
func healthySmall(host string) *types.Snapshot {
	return &types.Snapshot{Host: types.HostInfo{Hostname: host}}
}

// BenchmarkBodyBuffers_Decode is the aggregator's side of one peer poll: a
// production-sized body arriving in pieces, read and decoded. Its B/op is the
// figure #76 set out to reduce -- the per-cycle garbage on the smallest machine
// in the fleet -- and it excludes the HTTP round trip and the in-process peer
// that BenchmarkPoller_RefreshCPU cannot.
func BenchmarkBodyBuffers_Decode(b *testing.B) {
	body := encodeSnapshot(b, benchSnapshot("omega"))
	var bufs bodyBuffers

	b.SetBytes(int64(len(body)))
	for b.Loop() {
		var snap types.Snapshot
		if err := bufs.decode("omega", chunked(body), maxSnapshotBytes, &snap); err != nil {
			b.Fatal(err)
		}
	}
}
