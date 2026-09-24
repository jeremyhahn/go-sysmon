package fleet

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/config"
	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// countingPeers starts n peers serving a minimal summary, and counts the
// connections they accept between them.
func countingPeers(t *testing.T, n int) (*Poller, *atomic.Int64) {
	t.Helper()
	var dials atomic.Int64
	cfg := config.Fleet{PollIntervalMs: 3000, TimeoutMs: 3000}
	for i := range n {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(`{"hostname":"peer"}`)); err != nil {
				t.Errorf("writing summary: %v", err)
			}
		}))
		srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				dials.Add(1)
			}
		}
		srv.Start()
		t.Cleanup(srv.Close)
		cfg.Peers = append(cfg.Peers, config.Peer{Name: fmt.Sprintf("peer-%03d", i), URL: srv.URL})
	}
	p := New(
		func() config.Fleet { return cfg },
		func() (*types.Snapshot, *types.Snapshot, time.Duration) { return nil, nil, 0 },
		func() string { return "local" },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return p, &dials
}

// TestRefresh_KeepsAConnectionToEveryPeer is the fan-out regression (#15): past
// a hundred peers the default idle pool evicted connections between cycles, so
// every cycle redialled most of the fleet. A warm cycle must open none.
func TestRefresh_KeepsAConnectionToEveryPeer(t *testing.T) {
	p, dials := countingPeers(t, 150)
	cfg := p.cfgFn()
	p.refresh(cfg)
	dials.Store(0)

	for range 3 {
		p.refresh(cfg)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("3 warm cycles over 150 peers opened %d new connections; want 0 -- the idle pool is evicting peers", got)
	}
}

// TestPooled_LeavesTheSharedTransportAlone: the pool is widened on a clone.
// http.DefaultTransport is the whole process's, and a fleet setting leaking
// into it would change every other client in the binary.
func TestPooled_LeavesTheSharedTransportAlone(t *testing.T) {
	shared := http.DefaultTransport.(*http.Transport)
	before := shared.MaxIdleConns

	c := pooled(outbound.NewClient(0))
	own, ok := c.Transport.(*http.Transport)
	if !ok || own == shared {
		t.Fatalf("pooled client transport = %T %p; want its own *http.Transport", c.Transport, c.Transport)
	}
	if own.MaxIdleConns != 0 {
		t.Errorf("MaxIdleConns = %d; want 0, no cap across peers", own.MaxIdleConns)
	}
	if shared.MaxIdleConns != before {
		t.Errorf("http.DefaultTransport.MaxIdleConns changed from %d to %d", before, shared.MaxIdleConns)
	}
	if c.CheckRedirect == nil {
		t.Error("the redirect policy was lost in the copy")
	}
}

// TestPooled_KeepsACustomRoundTripper: a client whose transport is not an
// *http.Transport has nothing to widen, and is used as given.
func TestPooled_KeepsACustomRoundTripper(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, http.ErrUseLastResponse })
	c := &http.Client{Transport: rt}
	if got := pooled(c); got != c {
		t.Errorf("pooled replaced a client with a custom RoundTripper")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
